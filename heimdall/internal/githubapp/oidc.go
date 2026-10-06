package githubapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ActionsIssuer = "https://token.actions.githubusercontent.com"

type OIDCOptions struct {
	Audience   string
	HTTPClient *http.Client
	JWKSURL    string
	Now        func() time.Time
}
type OIDCVerifier struct {
	o       OIDCOptions
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
}
type ActionsIdentity struct {
	RepositoryID                         int64
	Repository                           string
	PullRequest                          int64
	SHA, WorkflowRef, WorkflowSHA, RunID string
	RunAttempt                           int64
	EventName                            string
}

func NewOIDCVerifier(o OIDCOptions) (*OIDCVerifier, error) {
	if o.Audience == "" {
		return nil, errors.New("GitHub Actions audience required")
	}
	if o.JWKSURL == "" {
		o.JWKSURL = ActionsIssuer + "/.well-known/jwks"
	}
	u, err := url.Parse(o.JWKSURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost"))) {
		return nil, errors.New("actions JWKS URL requires HTTPS (HTTP allowed only for loopback tests)")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	copyHTTP := *o.HTTPClient
	copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTPClient = &copyHTTP
	if o.Now == nil {
		o.Now = time.Now
	}
	return &OIDCVerifier{o: o, keys: map[string]*rsa.PublicKey{}}, nil
}
func (v *OIDCVerifier) Verify(ctx context.Context, token string) (ActionsIdentity, error) {
	var id ActionsIdentity
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 16<<10 {
		return id, errors.New("invalid Actions token")
	}
	decode := func(s string, out any) error {
		b, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil {
			return e
		}
		return json.Unmarshal(b, out)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if decode(parts[0], &header) != nil || header.Alg != "RS256" || header.Kid == "" {
		return id, errors.New("actions token must use RS256 and a key ID")
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return id, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return id, errors.New("invalid Actions signature")
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], sig) != nil {
		return id, errors.New("invalid Actions signature")
	}
	var c struct {
		Issuer       string          `json:"iss"`
		Audience     json.RawMessage `json:"aud"`
		Subject      string          `json:"sub"`
		Expiry       int64           `json:"exp"`
		NotBefore    int64           `json:"nbf"`
		IssuedAt     int64           `json:"iat"`
		RepositoryID string          `json:"repository_id"`
		Repository   string          `json:"repository"`
		Ref          string          `json:"ref"`
		SHA          string          `json:"sha"`
		Event        string          `json:"event_name"`
		WorkflowRef  string          `json:"job_workflow_ref"`
		WorkflowSHA  string          `json:"job_workflow_sha"`
		RunID        string          `json:"run_id"`
		RunAttempt   string          `json:"run_attempt"`
	}
	if decode(parts[1], &c) != nil {
		return id, errors.New("invalid Actions claims")
	}
	now := v.o.Now().Unix()
	var audience string
	_ = json.Unmarshal(c.Audience, &audience)
	if audience == "" {
		var a []string
		if json.Unmarshal(c.Audience, &a) == nil && len(a) == 1 {
			audience = a[0]
		}
	}
	if c.Issuer != ActionsIssuer || audience != v.o.Audience || c.Expiry <= now || c.NotBefore > now+30 || c.IssuedAt > now+30 || c.IssuedAt <= 0 || c.Expiry-c.IssuedAt > 15*60 {
		return id, errors.New("invalid Actions issuer, audience or validity")
	}
	defaultSubject := "repo:" + c.Repository + ":pull_request"
	workflowSubject := defaultSubject + ":job_workflow_ref:" + c.WorkflowRef
	if c.Event != "pull_request" || (c.Subject != defaultSubject && c.Subject != workflowSubject) {
		return id, errors.New("only pull-request Actions identities are accepted")
	}
	ref := strings.Split(c.Ref, "/")
	if len(ref) != 4 || ref[0] != "refs" || ref[1] != "pull" || ref[3] != "merge" {
		return id, errors.New("actions ref is not a pull request")
	}
	id.RepositoryID, err = strconv.ParseInt(c.RepositoryID, 10, 64)
	if err != nil || id.RepositoryID <= 0 {
		return id, errors.New("invalid Actions repository ID")
	}
	id.PullRequest, err = strconv.ParseInt(ref[2], 10, 64)
	if err != nil || id.PullRequest <= 0 {
		return id, errors.New("invalid Actions pull request")
	}
	id.RunAttempt, err = strconv.ParseInt(c.RunAttempt, 10, 64)
	run, e := strconv.ParseInt(c.RunID, 10, 64)
	if err != nil || e != nil || run <= 0 || id.RunAttempt <= 0 || !ValidSHA(c.SHA) || !ValidSHA(c.WorkflowSHA) || c.WorkflowRef == "" {
		return id, errors.New("incomplete Actions workflow identity")
	}
	id.Repository, id.SHA, id.WorkflowRef, id.WorkflowSHA, id.RunID, id.EventName = c.Repository, c.SHA, c.WorkflowRef, c.WorkflowSHA, c.RunID, c.Event
	return id, nil
}
func (v *OIDCVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.o.Now().Before(v.expires) {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", v.o.JWKSURL, nil)
	r, e := v.o.HTTPClient.Do(req)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, errors.New("GitHub Actions signing keys unavailable")
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&doc) != nil {
		return nil, errors.New("invalid GitHub Actions signing keys")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range doc.Keys {
		if j.Kty != "RSA" || (j.Alg != "" && j.Alg != "RS256") || (j.Use != "" && j.Use != "sig") {
			continue
		}
		n, e := base64.RawURLEncoding.DecodeString(j.N)
		if e != nil {
			continue
		}
		eb, e := base64.RawURLEncoding.DecodeString(j.E)
		if e != nil || len(eb) > 4 {
			continue
		}
		exponent := 0
		for _, b := range eb {
			exponent = (exponent << 8) | int(b)
		}
		if len(n) < 256 || exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
	}
	v.keys, v.expires = keys, v.o.Now().Add(15*time.Minute)
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, errors.New("unknown GitHub Actions signing key")
}

// VerifyBuild ties a signed OIDC job to GitHub's current PR and run metadata.
// SHA may be the PR merge commit; the run's PR link binds the actual head.
func (c *Client) VerifyBuild(ctx context.Context, r RepoRef, id ActionsIdentity, headSHA, workflowRef, workflowSHA string) (PullRequest, error) {
	if id.RepositoryID != r.RepositoryID || id.Repository != r.FullName || id.WorkflowRef != workflowRef || id.WorkflowSHA != workflowSHA || !ValidSHA(headSHA) {
		return PullRequest{}, errors.New("actions repository or trusted workflow mismatch")
	}
	pr, err := c.PullRequest(ctx, r, id.PullRequest)
	if err != nil {
		return pr, err
	}
	if pr.State != "open" || pr.Fork(r.RepositoryID) || pr.Head.SHA != headSHA {
		return pr, errors.New("build is stale, closed or from a fork")
	}
	runID, _ := strconv.ParseInt(id.RunID, 10, 64)
	run, err := c.WorkflowRun(ctx, r, runID)
	if err != nil {
		return pr, err
	}
	if run.ID != runID || run.RunAttempt != id.RunAttempt || run.Repository.ID != r.RepositoryID || run.HeadRepository.ID != r.RepositoryID || run.Event != "pull_request" || run.HeadSHA != id.SHA {
		return pr, errors.New("actions run identity mismatch")
	}
	for _, p := range run.PullRequests {
		if p.Number == pr.Number && p.Head.SHA == headSHA && p.Head.Repo.ID == r.RepositoryID {
			return pr, nil
		}
	}
	return pr, errors.New("actions run does not belong to this PR head")
}
