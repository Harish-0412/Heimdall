// Package githubapp implements the narrow GitHub App/Actions trust boundary.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v80/github"
)

var ErrNotFound = errors.New("GitHub resource not found")

type RepoRef struct {
	InstallationID, RepositoryID int64
	FullName                     string
}
type ClientOptions struct {
	AppID       int64
	PrivateKey  []byte
	BaseURL     string
	HTTPClient  *http.Client
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	MaxAttempts int
}
type cached struct {
	etag string
	body []byte
}
type Client struct {
	o        ClientOptions
	key      *rsa.PrivateKey
	mu       sync.Mutex
	cache    map[string]cached
	botLogin string
}
type APIError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("GitHub API returned HTTP %d", e.Status) }
func NewClient(o ClientOptions) (*Client, error) {
	b, _ := pem.Decode(o.PrivateKey)
	if b == nil {
		return nil, errors.New("GitHub App private key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		p, e := x509.ParsePKCS8PrivateKey(b.Bytes)
		if e != nil {
			return nil, errors.New("GitHub App private key is not RSA")
		}
		var ok bool
		key, ok = p.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App private key is not RSA")
		}
	}
	if o.AppID <= 0 {
		return nil, errors.New("GitHub App ID is required")
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.github.com/"
	}
	u, e := url.Parse(o.BaseURL)
	if e != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid GitHub API base URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) {
		return nil, errors.New("GitHub API requires HTTPS (HTTP allowed only for loopback tests)")
	}
	if !strings.HasSuffix(o.BaseURL, "/") {
		o.BaseURL += "/"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	// Never forward an installation credential across a redirect.
	copyHTTP := *o.HTTPClient
	copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTPClient = &copyHTTP
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 4
	}
	if o.MaxAttempts < 1 || o.MaxAttempts > 8 {
		return nil, errors.New("GitHub retry count must be 1..8")
	}
	return &Client{o: o, key: key, cache: map[string]cached{}}, nil
}
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (c *Client) appJWT() (string, error) {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	now := c.o.Now()
	p, _ := json.Marshal(map[string]any{"iss": strconv.FormatInt(c.o.AppID, 10), "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	s := head + "." + base64.RawURLEncoding.EncodeToString(p)
	hash := sha256.Sum256([]byte(s))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return s + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (c *Client) token(ctx context.Context, repo RepoRef, permissions map[string]string) (string, error) {
	if repo.InstallationID <= 0 || repo.RepositoryID <= 0 {
		return "", errors.New("GitHub installation/repository identity required")
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	var result struct {
		Token        string `json:"token"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
		Permissions map[string]string `json:"permissions"`
	}
	_, err = c.request(ctx, jwt, "POST", fmt.Sprintf("app/installations/%d/access_tokens", repo.InstallationID), map[string]any{"repository_ids": []int64{repo.RepositoryID}, "permissions": permissions}, &result, false)
	if err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", errors.New("GitHub returned an empty installation token")
	}
	for _, r := range result.Repositories {
		if r.ID != repo.RepositoryID {
			return "", errors.New("GitHub returned an over-broad repository token")
		}
	}
	for p, level := range result.Permissions {
		want, ok := permissions[p]
		if p == "metadata" && level == "read" {
			continue
		}
		if !ok || level != want {
			return "", errors.New("GitHub returned an over-broad permission token")
		}
	}
	return result.Token, nil
}
func (c *Client) do(ctx context.Context, repo RepoRef, perm map[string]string, method, path string, input, output any) error {
	if err := repo.Validate(); err != nil {
		return err
	}
	token, err := c.token(ctx, repo, perm)
	if err != nil {
		return err
	}
	_, err = c.request(ctx, token, method, path, input, output, method == "GET")
	return err
}
func (c *Client) request(ctx context.Context, token, method, path string, input, output any, useCache bool) (int, error) {
	if strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return 0, errors.New("GitHub path must be relative")
	}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return 0, err
		}
	}
	cacheKey := path
	c.mu.Lock()
	saved := c.cache[cacheKey]
	c.mu.Unlock()
	for attempt := 0; attempt < c.o.MaxAttempts; attempt++ {
		// go-github supplies the versioned request construction and API media type.
		g := gh.NewClient(c.o.HTTPClient)
		g.BaseURL, _ = url.Parse(c.o.BaseURL)
		req, err := g.NewRequest(method, path, nil)
		if err != nil {
			return 0, err
		}
		req = req.WithContext(ctx)
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		if useCache && saved.etag != "" {
			req.Header.Set("If-None-Match", saved.etag)
		}
		r, err := c.o.HTTPClient.Do(req)
		if err != nil {
			return 0, err
		}
		data, e := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		r.Body.Close()
		if e != nil {
			return r.StatusCode, e
		}
		if len(data) > 2<<20 {
			return r.StatusCode, errors.New("GitHub response exceeds limit")
		}
		if r.StatusCode == 304 && useCache && saved.body != nil {
			data = saved.body
		} else if r.StatusCode < 200 || r.StatusCode >= 300 {
			if r.StatusCode == 404 {
				return 404, ErrNotFound
			}
			delay, retry := c.retryDelay(r, attempt)
			safeRetry := method == "GET" || method == "PATCH" || strings.HasSuffix(path, "/access_tokens")
			if retry && safeRetry && attempt+1 < c.o.MaxAttempts {
				if e := c.o.Sleep(ctx, delay); e != nil {
					return r.StatusCode, e
				}
				continue
			}
			return r.StatusCode, &APIError{Status: r.StatusCode, RetryAfter: delay}
		}
		if useCache && r.StatusCode != 304 && r.Header.Get("ETag") != "" {
			c.mu.Lock()
			if len(c.cache) >= 128 {
				c.cache = map[string]cached{}
			}
			c.cache[cacheKey] = cached{r.Header.Get("ETag"), append([]byte(nil), data...)}
			c.mu.Unlock()
		}
		if output != nil && len(data) > 0 {
			if e := json.Unmarshal(data, output); e != nil {
				return r.StatusCode, errors.New("invalid GitHub response")
			}
		}
		return r.StatusCode, nil
	}
	return 0, errors.New("GitHub retries exhausted")
}
func (c *Client) retryDelay(r *http.Response, attempt int) (time.Duration, bool) {
	retry := r.StatusCode == 429 || r.StatusCode >= 500 || (r.StatusCode == 403 && (r.Header.Get("X-RateLimit-Remaining") == "0" || r.Header.Get("Retry-After") != ""))
	if !retry {
		return 0, false
	}
	d := time.Second * time.Duration(1<<attempt)
	if n, e := strconv.ParseInt(r.Header.Get("Retry-After"), 10, 64); e == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	if r.Header.Get("X-RateLimit-Remaining") == "0" {
		if n, e := strconv.ParseInt(r.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
			if reset := time.Unix(n, 0).Sub(c.o.Now()); reset > d {
				d = reset
			}
		}
	}
	if d > time.Minute {
		d = time.Minute
	}
	j, _ := rand.Int(rand.Reader, big.NewInt(500))
	return d + time.Duration(j.Int64())*time.Millisecond, true
}
func repoPath(r RepoRef) string { return "repos/" + strings.Trim(r.FullName, "/") + "/" }

// BotLogin identifies the actual App owner of recovery markers. A PR author
// cannot claim the marker in their own comment and induce an edit to it.
func (c *Client) BotLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	login := c.botLogin
	c.mu.Unlock()
	if login != "" {
		return login, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	_, err = c.request(ctx, jwt, "GET", "app", nil, &app, false)
	if err != nil {
		return "", err
	}
	if app.ID != c.o.AppID || app.Slug == "" {
		return "", errors.New("GitHub App identity mismatch")
	}
	login = app.Slug + "[bot]"
	c.mu.Lock()
	c.botLogin = login
	c.mu.Unlock()
	return login, nil
}
