// Package controlclient implements the cluster's outbound-only control channel.
package controlclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
)

// Session is stored in the cluster, never logged. Nonce is persisted before
// consuming the enrollment/refresh token, so a lost response can be replayed.
type Session struct {
	Enrollment string        `json:"enrollment,omitempty"`
	Nonce      string        `json:"nonce,omitempty"`
	Pair       gen.TokenPair `json:"pair"`
}

// SessionStore implements optimistic compare-and-swap across agent replicas.
type SessionStore interface {
	Load(context.Context) (Session, string, error)
	Save(context.Context, Session, string) error
}

type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control API rejected request (%d, %s)", e.Status, e.Code)
}

type Client struct {
	BaseURL, ClusterID, Version string
	HTTP                        *http.Client
	State                       SessionStore
	mu                          sync.Mutex
}

func New(baseURL, clusterID, version string, state SessionStore, allowLocalHTTP bool) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("control plane URL must be an HTTPS origin")
	}
	if u.Scheme != "https" && (!allowLocalHTTP || u.Scheme != "http") {
		return nil, errors.New("control plane requires HTTPS (HTTP is an explicit local-test option)")
	}
	if clusterID == "" || state == nil {
		return nil, errors.New("cluster identity and durable session store are required")
	}
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), ClusterID: clusterID, Version: version, State: state,
		HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) request(ctx context.Context, method, path, token, nonce string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	tracecontext.Inject(ctx, r.Header)
	if in != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if nonce != "" {
		r.Header.Set("Idempotency-Key", nonce)
	}
	rsp, err := c.HTTP.Do(r)
	if err != nil {
		return errors.New("control API connection failed")
	}
	defer rsp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(rsp.Body, 8<<20+1))
	if err != nil || len(data) > 8<<20 {
		return errors.New("control API response exceeds limit or is unavailable")
	}
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &e)
		return &HTTPError{Status: rsp.StatusCode, Code: e.Code}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("control API returned invalid JSON")
		}
	}
	return nil
}

func (c *Client) access(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 8; attempt++ {
		s, rv, err := c.State.Load(ctx)
		if err != nil {
			return "", err
		}
		if s.Pair.ClusterID != "" && s.Pair.ClusterID != c.ClusterID {
			return "", errors.New("stored credentials belong to a different cluster")
		}
		if s.Pair.AccessToken != "" && time.Until(s.Pair.AccessExpiresAt) > time.Minute && s.Nonce == "" {
			return s.Pair.AccessToken, nil
		}
		if s.Nonce == "" {
			b := make([]byte, 32)
			if _, err = rand.Read(b); err != nil {
				return "", err
			}
			s.Nonce = hex.EncodeToString(b)
			if err = c.State.Save(ctx, s, rv); err != nil {
				continue
			}
			continue
		}
		endpoint, token, input := "/v1/agent/refresh", s.Pair.RefreshToken, any(nil)
		if token == "" {
			endpoint, token, input = "/v1/agent/register", s.Enrollment, gen.AgentRegistration{ClusterID: c.ClusterID, Version: c.Version}
		}
		if token == "" {
			return "", errors.New("no enrollment or refresh credential; cluster enrollment is required")
		}
		var pair gen.TokenPair
		if err = c.request(ctx, "POST", endpoint, token, s.Nonce, input, &pair); err != nil {
			return "", err
		}
		if pair.ClusterID != c.ClusterID || pair.AccessToken == "" || pair.RefreshToken == "" || !pair.AccessExpiresAt.After(time.Now()) || !pair.RefreshExpiresAt.After(pair.AccessExpiresAt) {
			return "", errors.New("invalid cluster token pair")
		}
		s.Pair, s.Enrollment, s.Nonce = pair, "", ""
		if err = c.State.Save(ctx, s, rv); err != nil {
			continue
		}
		return pair.AccessToken, nil
	}
	return "", errors.New("cluster session changed too frequently; retry later")
}

// Do sends a bounded authenticated request. It never follows redirects or
// puts credentials in error messages. Rejected credentials fail closed.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	if !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "?#\r\n") {
		return errors.New("invalid control API path")
	}
	token, err := c.access(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, path, token, "", in, out)
}

func (c *Client) Desired(ctx context.Context) (gen.DesiredSnapshot, error) {
	var s gen.DesiredSnapshot
	err := c.Do(ctx, "GET", "/v1/agent/desired", nil, &s)
	return s, err
}
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.Do(ctx, "POST", "/v1/agent/heartbeat", gen.Heartbeat{Version: c.Version}, nil)
}
