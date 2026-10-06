package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const tenantA = "11111111-1111-4111-8111-111111111111"
const tenantB = "22222222-2222-4222-8222-222222222222"
const clusterA = "33333333-3333-4333-8333-333333333333"
const clusterB = "44444444-4444-4444-8444-444444444444"
const repoA = "55555555-5555-4555-8555-555555555555"
const apiImage = "ghcr.io/acme/app@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const apiDoc = "version: 1\nservices:\n  app: {image: " + apiImage + ", port: 80, public: true, health: {path: /h}}\npreview: {visibility: private, ttl: 48h}\n"

// Embed the interface so any unexpected call fails the HTTP request visibly.
// Each test supplies behavior at the boundary it exercises.
type apiRepository struct {
	Repository
	mu                                           sync.Mutex
	tokens                                       map[string]domain.Principal
	envs                                         []domain.Environment
	policy                                       config.Policy
	audits                                       []json.RawMessage
	status                                       domain.StatusUpdate
	statusErr                                    error
	action                                       domain.Action
	actionErr                                    error
	createErr                                    error
	exchangeKind, exchangeCluster, exchangeNonce string
	exchangeErr                                  error
	authCalls                                    int
	authLimit                                    int
	timelineCalls                                int
	events                                       []domain.Event
}

func apiFixture() *apiRepository {
	p := config.DefaultPolicy()
	p.AllowedSecrets = []string{}
	p.AllowedRegistries = []string{"ghcr.io/acme/"}
	base := domain.Principal{TenantID: tenantA, TenantSlug: "acme", ActorID: "octocat", Role: "admin", Kind: "user"}
	tokens := map[string]domain.Principal{"admin": base}
	for _, role := range []string{"member", "viewer"} {
		q := base
		q.Role = role
		tokens[role] = q
	}
	q := base
	q.Role = "agent"
	q.Kind = "access"
	q.ClusterID = clusterA
	tokens["agent"] = q
	q.ClusterID = clusterB
	tokens["other-cluster"] = q
	q = base
	q.TenantID = tenantB
	q.TenantSlug = "other"
	tokens["other-tenant"] = q
	q = base
	q.Kind = "enrollment"
	tokens["enrollment"] = q
	q = base
	q.Kind = "ci"
	tokens["ci"] = q
	spec, _ := json.Marshal(apiSpec())
	now := time.Now().UTC()
	return &apiRepository{tokens: tokens, policy: p, envs: []domain.Environment{{ID: "env-7", TenantID: tenantA, ClusterID: clusterA, RepositoryID: repoA, Name: "env-7", Version: 1, Generation: 1, DesiredState: "Running", Phase: "Pending", Spec: spec, Status: json.RawMessage(`{}`), ExpiresAt: now.Add(48 * time.Hour), CreatedAt: now, UpdatedAt: now}}}
}

func apiSpec() v1alpha1.PreviewEnvironmentSpec {
	sum := sha256.Sum256([]byte(apiDoc))
	return v1alpha1.PreviewEnvironmentSpec{Tenant: "acme", Repository: "acme/demo", PullRequest: 7, Commit: strings.Repeat("a", 40), Generation: 1, EnvironmentID: "env-7", Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now().Add(48 * time.Hour)), DesiredState: v1alpha1.DesiredRunning, Config: v1alpha1.ConfigSource{Inline: apiDoc, SHA256: hex.EncodeToString(sum[:])}}
}

func (f *apiRepository) Ping(context.Context) error { return nil }
func (f *apiRepository) Authenticate(_ context.Context, token string) (domain.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	p, ok := f.tokens[token]
	if !ok || (f.authLimit > 0 && f.authCalls > f.authLimit) {
		return domain.Principal{}, domain.ErrUnauthorized
	}
	return p, nil
}
func (f *apiRepository) GetEnvironment(_ context.Context, p domain.Principal, id string) (domain.Environment, error) {
	for _, e := range f.envs {
		if e.ID == id && e.TenantID == p.TenantID && (p.Role != "agent" || e.ClusterID == p.ClusterID) {
			return e, nil
		}
	}
	return domain.Environment{}, domain.ErrNotFound
}
func (f *apiRepository) ListEnvironments(_ context.Context, p domain.Principal, page domain.Page) ([]domain.Environment, error) {
	out := []domain.Environment{}
	for _, e := range f.envs {
		if e.TenantID == p.TenantID && e.ID > page.AfterID {
			out = append(out, e)
			if len(out) == page.Limit {
				break
			}
		}
	}
	return out, nil
}
func (f *apiRepository) GetPolicy(context.Context, domain.Principal) (config.Policy, error) {
	return f.policy, nil
}
func (f *apiRepository) SetPolicy(_ context.Context, _ domain.Principal, p config.Policy) error {
	f.policy = p
	return nil
}
func (f *apiRepository) Snapshot(_ context.Context, p domain.Principal) (domain.DesiredSnapshot, error) {
	out := []domain.Environment{}
	for _, e := range f.envs {
		if e.TenantID == p.TenantID && e.ClusterID == p.ClusterID {
			out = append(out, e)
		}
	}
	return domain.DesiredSnapshot{Revision: 7, Environments: out, Policy: f.policy}, nil
}
func (f *apiRepository) GetRepository(_ context.Context, p domain.Principal, id string) (domain.Repository, error) {
	if p.TenantID != tenantA || id != repoA {
		return domain.Repository{}, domain.ErrNotFound
	}
	return domain.Repository{ID: repoA, TenantID: tenantA, ClusterID: clusterA, FullName: "acme/demo", Enabled: true}, nil
}
func (f *apiRepository) GetCluster(_ context.Context, p domain.Principal, id string) (domain.Cluster, error) {
	if p.TenantID != tenantA || id != clusterA {
		return domain.Cluster{}, domain.ErrNotFound
	}
	return domain.Cluster{ID: clusterA, TenantID: tenantA, Name: "demo"}, nil
}
func (f *apiRepository) CreateEnvironment(_ context.Context, _ domain.Principal, in domain.CreateEnvironment, _ string) (domain.Environment, error) {
	if f.createErr != nil {
		return domain.Environment{}, f.createErr
	}
	e := f.envs[0]
	e.Spec = in.Spec
	return e, nil
}
func (f *apiRepository) ActEnvironment(_ context.Context, _ domain.Principal, _ string, _ int64, a domain.Action, _ string) (domain.Environment, error) {
	f.action = a
	if f.actionErr != nil {
		return domain.Environment{}, f.actionErr
	}
	return f.envs[0], nil
}
func (f *apiRepository) ReportStatus(_ context.Context, p domain.Principal, id string, u domain.StatusUpdate) (domain.Environment, error) {
	e, err := f.GetEnvironment(context.Background(), p, id)
	if err != nil {
		return e, err
	}
	f.status = u
	if f.statusErr != nil {
		return e, f.statusErr
	}
	e.Status = u.Status
	e.Phase = u.Phase
	e.Version++
	return e, nil
}
func (f *apiRepository) ExchangeCredentialWithNonce(_ context.Context, _ string, kind, cluster, nonce string) (domain.CredentialPair, error) {
	f.exchangeKind = kind
	f.exchangeCluster = cluster
	f.exchangeNonce = nonce
	if f.exchangeErr != nil {
		return domain.CredentialPair{}, f.exchangeErr
	}
	return domain.CredentialPair{Access: domain.IssuedCredential{Credential: domain.Credential{ClusterID: clusterA, ExpiresAt: time.Now().Add(15 * time.Minute)}, Token: "access"}, Refresh: domain.IssuedCredential{Credential: domain.Credential{ClusterID: clusterA, ExpiresAt: time.Now().Add(24 * time.Hour)}, Token: "refresh"}}, nil
}
func (f *apiRepository) AppendAudit(_ context.Context, _ domain.Principal, _, _ string, raw json.RawMessage) error {
	f.audits = append(f.audits, raw)
	return nil
}
func (f *apiRepository) Timeline(_ context.Context, p domain.Principal, id string, after int64, limit int) ([]domain.Event, error) {
	if _, err := f.GetEnvironment(context.Background(), p, id); err != nil {
		return nil, err
	}
	f.timelineCalls++
	out := []domain.Event{}
	for _, e := range f.events {
		if e.ID > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func apiRequest(t *testing.T, s *Server, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestHTTPAuthenticationAndScope(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{})
	for _, tc := range []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"missing bearer", "GET", "/v1/environments", "", "", 401},
		{"invalid bearer", "GET", "/v1/environments", "missing", "", 401},
		{"enrollment cannot read", "GET", "/v1/environments", "enrollment", "", 403},
		{"CI cannot read", "GET", "/v1/environments", "ci", "", 403},
		{"viewer reads", "GET", "/v1/environments/env-7", "viewer", "", 200},
		{"cross tenant", "GET", "/v1/environments/env-7", "other-tenant", "", 404},
		{"agent reads scoped version for CAS recovery", "GET", "/v1/environments/env-7", "agent", "", 200},
		{"agent cannot read another cluster", "GET", "/v1/environments/env-7", "other-cluster", "", 404},
		{"agent cannot mutate intent", "POST", "/v1/environments/env-7/actions", "agent", `{"action":"delete","version":1}`, 403},
		{"viewer cannot mutate intent", "POST", "/v1/environments/env-7/actions", "viewer", `{"action":"delete","version":1}`, 403},
		{"member cannot attest raw desired spec", "POST", "/v1/environments", "member", `{}`, 403},
		{"member cannot edit policy", "PUT", "/v1/policy", "member", `{}`, 403},
		{"agent scope", "GET", "/v1/agent/desired", "other-cluster", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiRequest(t, s, tc.method, tc.path, tc.token, tc.body, map[string]string{"Idempotency-Key": "test-operation", "X-Tenant-ID": tenantA})
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing safe response headers")
			}
		})
	}
	if w := apiRequest(t, s, "GET", "/v1/agent/desired", "other-cluster", "", nil); !strings.Contains(w.Body.String(), `"environments":[]`) {
		t.Fatal("other cluster received desired objects")
	}
}

func TestStrictJSONRejectsAmbiguousDocuments(t *testing.T) {
	type nested struct {
		Name string `json:"name"`
	}
	type doc struct {
		Value    nested  `json:"value"`
		Optional *string `json:"optional,omitempty"`
	}
	for _, body := range []string{`{"value":{"name":"one","name":"two"}}`, `{"value":{"Name":"one"}}`, `{"value":{"name":"one","extra":1}}`, `{"value":{"name":null}}`, `{"value":{}}`, `{"value":{"name":"one"},"optional":null}`, `{"value":{"name":"one"}} {}`, `null`, `{"value":{"name":"` + string([]byte{255}) + `"}}`} {
		var out doc
		if StrictJSON([]byte(body), &out) == nil {
			t.Fatalf("accepted ambiguous document: %q", body)
		}
	}
	var out doc
	if err := StrictJSON([]byte(`{"value":{"name":"one"}}`), &out); err != nil {
		t.Fatal(err)
	}
	var raw any
	if StrictJSON([]byte(strings.Repeat("[", 66)+"1"+strings.Repeat("]", 66)), &raw) == nil {
		t.Fatal("accepted excessive JSON depth")
	}
}

func TestHTTPBodyAndRequiredFields(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{})
	for _, tc := range []struct {
		body, media string
		want        int
	}{
		{`{"name":"demo"}`, "application/json", 400},
		{`{"name":"demo","tier":0,"extra":true}`, "application/json", 400},
		{`{"name":"demo","tier":0}`, "text/plain", 415},
		{`{"name":"demo","tier":0}` + strings.Repeat(" ", MaxBodyBytes), "application/json", 413},
	} {
		w := apiRequest(t, s, "POST", "/v1/clusters", "admin", tc.body, map[string]string{"Content-Type": tc.media})
		if w.Code != tc.want {
			t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
		}
	}
}

func TestCredentialExchangeRequiresDurableNonce(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{})
	for _, path := range []string{"/v1/agent/register", "/v1/agent/refresh"} {
		body := ""
		kind := "refresh"
		if strings.HasSuffix(path, "register") {
			kind = "enrollment"
			body = `{"clusterID":"` + clusterA + `","version":"0.5.0"}`
		}
		w := apiRequest(t, s, "POST", path, "opaque-bootstrap", body, nil)
		if w.Code != 400 {
			t.Fatalf("nonce missing: %d %s", w.Code, w.Body.String())
		}
		w = apiRequest(t, s, "POST", path, "opaque-bootstrap", body, map[string]string{"Idempotency-Key": "persisted-nonce-42"})
		if w.Code != 200 || f.exchangeNonce != "persisted-nonce-42" || f.exchangeKind != kind {
			t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
		}
	}
	f.exchangeErr = domain.ErrUnauthorized
	w := apiRequest(t, s, "POST", "/v1/agent/refresh", "consumed", "", map[string]string{"Idempotency-Key": "different-nonce"})
	if w.Code != 401 {
		t.Fatal(w.Body.String())
	}
}

func TestCreateEnvironmentTrustAndIdempotencyErrors(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{})
	in := gen.CreateEnvironment{ClusterID: clusterA, RepositoryID: repoA, Name: "env-7"}
	in.Spec, _ = json.Marshal(apiSpec())
	body, _ := json.Marshal(in)
	w := apiRequest(t, s, "POST", "/v1/environments", "admin", string(body), map[string]string{"Idempotency-Key": "create-environment"})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	f.createErr = domain.ErrIdempotencyConflict
	w = apiRequest(t, s, "POST", "/v1/environments", "admin", string(body), map[string]string{"Idempotency-Key": "create-environment"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "api.idempotency_conflict") {
		t.Fatal(w.Body.String())
	}
	bad := apiSpec()
	bad.Tenant = "other"
	in.Spec, _ = json.Marshal(bad)
	body, _ = json.Marshal(in)
	w = apiRequest(t, s, "POST", "/v1/environments", "admin", string(body), map[string]string{"Idempotency-Key": "create-environment"})
	if w.Code != 400 {
		t.Fatal(w.Body.String())
	}
}

func TestStatusBoundariesAndRedactedSummaries(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{})
	st := v1alpha1.PreviewEnvironmentStatus{Phase: v1alpha1.PhaseReady, DeployedGeneration: 1, Steps: []v1alpha1.StepStatus{{Name: "smoke/run", State: "succeeded", DurationSeconds: 2}}, Diagnoses: []v1alpha1.DiagnosisStatus{{Code: "TEST", Summary: "password=shouldneverleave", Suggestion: "retry", Evidence: []string{"token=secret-value"}}}}
	raw, _ := json.Marshal(st)
	in := gen.StatusUpdate{Generation: 1, Version: 1, EventID: "event-001", Phase: gen.StatusUpdatePhase("Ready"), Status: raw}
	body, _ := json.Marshal(in)
	w := apiRequest(t, s, "POST", "/v1/agent/environments/env-7/status", "other-cluster", string(body), nil)
	if w.Code != 404 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "POST", "/v1/agent/environments/env-7/status", "agent", string(body), nil)
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(f.status.Status), "shouldneverleave") || strings.Contains(string(f.status.Status), "secret-value") || len(f.status.SmokeRuns) != 1 || !f.status.SmokeRuns[0].Passed {
		t.Fatalf("unsafe or missing status summaries: %+v", f.status)
	}
	f.statusErr = domain.ErrStaleGeneration
	w = apiRequest(t, s, "POST", "/v1/agent/environments/env-7/status", "agent", string(body), nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "state.stale_generation") {
		t.Fatal(w.Body.String())
	}
	st.DeployedGeneration = 2
	in.Status, _ = json.Marshal(st)
	body, _ = json.Marshal(in)
	w = apiRequest(t, s, "POST", "/v1/agent/environments/env-7/status", "agent", string(body), nil)
	if w.Code != 400 {
		t.Fatal(w.Body.String())
	}
}

func TestSSETimelineResumesAndReauthorizesEveryBatch(t *testing.T) {
	f := apiFixture()
	for i := int64(1); i <= 201; i++ {
		f.events = append(f.events, domain.Event{ID: i, EnvironmentID: "env-7", Generation: 1, Kind: "stage", Payload: json.RawMessage(`{"stage":"smoke"}`), CreatedAt: time.Now()})
	}
	f.authLimit = 2 // initial HTTP auth + first batch; revoke before second batch.
	s := httptest.NewServer(New(f, Options{StreamDuration: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond}).Handler())
	defer s.Close()
	r, _ := http.NewRequest("GET", s.URL+"/v1/environments/env-7/timeline?after=0", nil)
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || strings.Contains(string(body), "id: 1\n") || !strings.Contains(string(body), "id: 2\n") || strings.Contains(string(body), "id: 102\n") || f.timelineCalls != 1 {
		t.Fatalf("bad resume or revocation: calls %d body %s", f.timelineCalls, body)
	}
}

func TestKeysetPagesAndTypedStorageFailures(t *testing.T) {
	f := apiFixture()
	e := f.envs[0]
	e.ID = "env-8"
	f.envs = append(f.envs, e)
	s := New(f, Options{})
	w := apiRequest(t, s, "GET", "/v1/environments?limit=1", "viewer", "", nil)
	var page gen.EnvironmentPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Next == nil || *page.Next != "env-7" {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "GET", "/v1/environments?limit=1&after=env-7", "viewer", "", nil)
	page = gen.EnvironmentPage{}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].Id != "env-8" || page.Next != nil {
		t.Fatal(w.Body.String())
	}
	for _, err := range []error{domain.ErrConflict, domain.ErrInvalidTransition, domain.ErrIdempotencyConflict, domain.ErrQuotaExceeded} {
		f.actionErr = err
		w = apiRequest(t, s, "POST", "/v1/environments/env-7/actions", "admin", `{"action":"retry","version":1}`, map[string]string{"Idempotency-Key": "action-replay-42"})
		if w.Code != 409 {
			t.Fatal(w.Body.String())
		}
	}
	f.actionErr = errors.New("postgresql://user:top-secret@db/internal table details")
	w = apiRequest(t, s, "POST", "/v1/environments/env-7/actions", "admin", `{"action":"retry","version":1}`, map[string]string{"Idempotency-Key": "action-replay-42"})
	if w.Code != 503 || strings.Contains(w.Body.String(), "top-secret") {
		t.Fatal(w.Body.String())
	}
}
