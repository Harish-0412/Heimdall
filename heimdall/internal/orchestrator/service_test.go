package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
	"github.com/heimdall-dev/heimdall/internal/queue"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
)

var testHead = strings.Repeat("a", 40)
var testBase = strings.Repeat("b", 40)
var testWorkflow = strings.Repeat("c", 40)
var testImage = "registry.test/team/api@sha256:" + strings.Repeat("d", 64)
var testBundle = "registry.test/team/bundle@sha256:" + strings.Repeat("e", 64)

const testConfig = "version: 1\nservices:\n  api:\n    image: IMAGE\n    port: 8080\npreview:\n  ttl: 2h\n  visibility: private\n"

type memoryStore struct {
	Store
	repo         domain.Repository
	policy       config.Policy
	env          domain.Environment
	pr           domain.PullRequest
	deliveries   map[string][]byte
	done         map[string]bool
	observations []domain.PullRequestObservation
	commands     []string
	actions      []domain.Action
	builds       []domain.BuildInput
	completed    []int64
	delivery     domain.GitHubDelivery
}

func (m *memoryStore) ResolveInstallation(_ context.Context, id int64) (string, error) {
	if id != 9 {
		return "", domain.ErrNotFound
	}
	return "tenant", nil
}
func (m *memoryStore) ActiveTenants(context.Context) ([]string, error) {
	return []string{"tenant"}, nil
}
func (m *memoryStore) RepositoryByGitHubID(_ context.Context, p domain.Principal, id int64) (domain.Repository, error) {
	if p.TenantID != "tenant" || id != 7 {
		return domain.Repository{}, domain.ErrNotFound
	}
	return m.repo, nil
}
func (m *memoryStore) GetRepository(_ context.Context, p domain.Principal, id string) (domain.Repository, error) {
	if p.TenantID != "tenant" || id != m.repo.ID {
		return domain.Repository{}, domain.ErrNotFound
	}
	return m.repo, nil
}
func (m *memoryStore) GetTenant(context.Context, domain.Principal) (domain.Tenant, error) {
	return domain.Tenant{ID: "tenant", Slug: "team"}, nil
}
func (m *memoryStore) GetPolicy(context.Context, domain.Principal) (config.Policy, error) {
	return m.policy, nil
}
func (m *memoryStore) IngestWebhook(_ context.Context, _ domain.Principal, d domain.WebhookDelivery) (bool, error) {
	if saved, ok := m.deliveries[d.DeliveryID]; ok {
		if string(saved) != string(d.Payload) {
			return false, domain.ErrIdempotencyConflict
		}
		return false, nil
	}
	m.deliveries[d.DeliveryID] = d.Payload
	return true, nil
}
func (m *memoryStore) ClaimWebhook(_ context.Context, _ domain.Principal, id string) (domain.WebhookDelivery, error) {
	if m.done[id] {
		return domain.WebhookDelivery{}, domain.ErrNotFound
	}
	return domain.WebhookDelivery{DeliveryID: id, LeaseToken: "lease"}, nil
}
func (m *memoryStore) CompleteWebhook(_ context.Context, _ domain.Principal, id, _ string) error {
	m.done[id] = true
	return nil
}
func (m *memoryStore) RetryWebhook(context.Context, domain.Principal, string, string, string) error {
	return nil
}
func (m *memoryStore) ObservePullRequest(_ context.Context, _ domain.Principal, o domain.PullRequestObservation) (domain.Environment, error) {
	if m.pr.HeadSHA != o.HeadSHA || m.pr.State != o.State || m.pr.ConfigDigest != o.ConfigDigest || m.pr.BaselineDigest != o.BaselineDigest || m.pr.BaseSHA != o.BaseSHA {
		m.pr.TraceParent = o.TraceParent
	}
	m.pr.Version++
	m.observations = append(m.observations, o)
	m.pr.RepositoryID = o.RepositoryID
	m.pr.Number = o.Number
	m.pr.HeadSHA = o.HeadSHA
	m.pr.BaseSHA = o.BaseSHA
	m.pr.State = o.State
	m.pr.Fork = o.IsFork
	m.pr.NeedsApproval = o.NeedsApproval
	m.pr.ConfigDigest = o.ConfigDigest
	m.pr.BaselineDigest = o.BaselineDigest
	if o.State == "open" && m.env.ID == "" {
		m.env = domain.Environment{ID: "env-12345678", TenantID: "tenant", RepositoryID: m.repo.ID, PullRequest: o.Number, Commit: o.HeadSHA, Generation: 1, Version: 1, DesiredState: "Running", Phase: "Pending", BuildState: "pending", Spec: json.RawMessage(`{}`), ExpiresAt: time.Now().Add(time.Hour)}
	}
	if o.State == "closed" && m.env.ID != "" {
		m.env.Generation++
		m.env.DesiredState = "Destroyed"
		m.env.Phase = "Destroying"
	}
	if o.DeliveryID != "" {
		m.done[o.DeliveryID] = true
	}
	return m.env, nil
}
func (m *memoryStore) GetPullRequest(context.Context, domain.Principal, string, int64) (domain.PullRequest, error) {
	return m.pr, nil
}
func (m *memoryStore) EnvironmentByPullRequest(context.Context, domain.Principal, string, int64) (domain.Environment, error) {
	if m.env.ID == "" {
		return m.env, domain.ErrNotFound
	}
	return m.env, nil
}
func (m *memoryStore) ActEnvironment(_ context.Context, _ domain.Principal, _ string, _ int64, a domain.Action, _ string) (domain.Environment, error) {
	m.actions = append(m.actions, a)
	m.done[a.DeliveryID] = true
	return m.env, nil
}
func (m *memoryStore) ApprovePullRequest(_ context.Context, _ domain.Principal, _ string, _ int64, head, id, _ string) error {
	if !m.pr.NeedsApproval {
		return domain.ErrInvalidTransition
	}
	m.pr.ApprovedSHA = head
	m.done[id] = true
	return nil
}
func (m *memoryStore) CommitBuild(_ context.Context, _ domain.Principal, b domain.BuildInput) (domain.Environment, error) {
	m.builds = append(m.builds, b)
	m.env.Spec = b.Spec
	m.env.BuildState = "ready"
	return m.env, nil
}
func (m *memoryStore) RecordCommand(_ context.Context, _ domain.Principal, id, _, _ string, _, _ int64, command, outcome, response string) error {
	m.commands = append(m.commands, command+":"+outcome+":"+response)
	m.done[id] = true
	return nil
}
func (m *memoryStore) WithGitHubDelivery(_ context.Context, _ domain.Principal, _ string, fn func(domain.Environment, domain.GitHubDelivery) (domain.GitHubDelivery, error)) error {
	d, err := fn(m.env, m.delivery)
	if err == nil {
		m.delivery = d
	}
	return err
}
func (m *memoryStore) WithGitHubNotice(_ context.Context, _ domain.Principal, _ string, _ int64, fn func(domain.Repository, domain.PullRequest, *domain.Environment) error) error {
	var e *domain.Environment
	if m.env.ID != "" {
		e = &m.env
	}
	return fn(m.repo, m.pr, e)
}
func (m *memoryStore) FinishOutbox(_ context.Context, _ domain.Principal, id int64, _ string, _ int64, _ domain.GitHubDelivery) error {
	m.completed = append(m.completed, id)
	return nil
}

type fakeGitHub struct {
	GitHubAPI
	pr                                         githubapp.PullRequest
	raw, baseline                              []byte
	permission                                 string
	comment                                    githubapp.IssueComment
	comments                                   []githubapp.IssueComment
	checks                                     []githubapp.Check
	commentCreates, commentEdits, checkCreates int
	checkFailure                               bool
}

func (g *fakeGitHub) Repository(context.Context, githubapp.RepoRef) (githubapp.Repository, error) {
	return githubapp.Repository{ID: 7, FullName: "team/shop", DefaultBranch: "main"}, nil
}
func (g *fakeGitHub) PullRequest(context.Context, githubapp.RepoRef, int64) (githubapp.PullRequest, error) {
	return g.pr, nil
}
func (g *fakeGitHub) DefaultBranchSHA(context.Context, githubapp.RepoRef, string) (string, error) {
	return testBase, nil
}
func (g *fakeGitHub) Config(_ context.Context, _ githubapp.RepoRef, sha string) ([]byte, error) {
	if sha == testBase {
		if g.baseline == nil {
			return nil, githubapp.ErrNotFound
		}
		return g.baseline, nil
	}
	return g.raw, nil
}
func (g *fakeGitHub) Permission(context.Context, githubapp.RepoRef, string) (string, error) {
	return g.permission, nil
}
func (g *fakeGitHub) Comment(_ context.Context, _ githubapp.RepoRef, id int64) (githubapp.IssueComment, error) {
	for _, c := range g.comments {
		if c.ID == id {
			return c, nil
		}
	}
	return g.comment, nil
}
func (g *fakeGitHub) VerifyBuild(_ context.Context, _ githubapp.RepoRef, id githubapp.ActionsIdentity, head, workflowRef, workflowSHA string) (githubapp.PullRequest, error) {
	if head != g.pr.Head.SHA || g.pr.Fork(7) || g.pr.State != "open" || id.WorkflowRef != workflowRef || id.WorkflowSHA != workflowSHA {
		return g.pr, errors.New("identity rejected")
	}
	return g.pr, nil
}
func (g *fakeGitHub) BotLogin(context.Context) (string, error) { return "heimdall[bot]", nil }
func (g *fakeGitHub) ListComments(context.Context, githubapp.RepoRef, int64) ([]githubapp.IssueComment, error) {
	return g.comments, nil
}
func (g *fakeGitHub) WriteComment(_ context.Context, _ githubapp.RepoRef, _ int64, id int64, body string) (int64, error) {
	if id == 0 {
		g.commentCreates++
		id = int64(len(g.comments) + 10)
		c := githubapp.IssueComment{ID: id, Body: body}
		c.User.Login = "heimdall[bot]"
		g.comments = append(g.comments, c)
	} else {
		g.commentEdits++
		for i := range g.comments {
			if g.comments[i].ID == id {
				g.comments[i].Body = body
			}
		}
	}
	return id, nil
}
func (g *fakeGitHub) Checks(context.Context, githubapp.RepoRef, string) ([]githubapp.Check, error) {
	return g.checks, nil
}
func (g *fakeGitHub) OwnCheck(c githubapp.Check) bool { return c.App.ID == 3 }
func (g *fakeGitHub) WriteCheck(_ context.Context, _ githubapp.RepoRef, id int64, u githubapp.CheckUpdate) (int64, error) {
	if g.checkFailure {
		g.checkFailure = false
		return 0, errors.New("crashed before ID persisted")
	}
	if id == 0 {
		g.checkCreates++
		id = int64(len(g.checks) + 20)
		c := githubapp.Check{ID: id, ExternalID: u.ExternalID}
		c.App.ID = 3
		g.checks = append(g.checks, c)
	}
	return id, nil
}

type fakeIdentity struct {
	id  githubapp.ActionsIdentity
	err error
}

func (v fakeIdentity) Verify(context.Context, string) (githubapp.ActionsIdentity, error) {
	return v.id, v.err
}
func setup(t *testing.T) (*Service, *memoryStore, *fakeGitHub) {
	t.Helper()
	raw := []byte(strings.ReplaceAll(testConfig, "IMAGE", testImage))
	g := &fakeGitHub{raw: raw, baseline: raw, permission: "write"}
	g.pr.Number = 101
	g.pr.State = "open"
	g.pr.Head.SHA = testHead
	g.pr.Head.Repo = &githubapp.Repository{ID: 7}
	g.pr.Base.Repo = &githubapp.Repository{ID: 7}
	g.pr.User.Login = "alice"
	g.pr.UpdatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	p := config.DefaultPolicy()
	p.AllowedSecrets = []string{}
	p.AllowedRegistries = []string{"registry.test/team/"}
	m := &memoryStore{repo: domain.Repository{ID: "repo", GitHubID: 7, InstallationID: 9, FullName: "team/shop", Enabled: true, TrustedWorkflowRef: "platform/heimdall/.github/workflows/preview.yml@" + testWorkflow, TrustedWorkflowSHA: testWorkflow}, policy: p, deliveries: map[string][]byte{}, done: map[string]bool{}}
	id := githubapp.ActionsIdentity{RepositoryID: 7, Repository: "team/shop", PullRequest: 101, SHA: testHead, WorkflowRef: m.repo.TrustedWorkflowRef, WorkflowSHA: testWorkflow, RunID: "42", RunAttempt: 1}
	s, err := New(Options{Store: m, GitHub: g, OIDC: fakeIdentity{id: id}, Now: func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return s, m, g
}
func message(id, event string) queue.Message {
	return queue.Message{DeliveryID: id, Event: event, InstallationID: 9, RepositoryID: 7, PullRequest: 101, Payload: json.RawMessage(`{"comment":{"id":11},"pull_request":{"head":{"sha":"forged"}}}`)}
}
func TestCanonicalWebhookAndDurableReplay(t *testing.T) {
	s, m, g := setup(t)
	ctx := context.Background()
	event := message("open-1", "pull_request")
	if err := s.Process(ctx, event); err != nil {
		t.Fatal(err)
	}
	if m.env.Commit != testHead || len(m.observations) != 1 {
		t.Fatal("webhook payload became authority")
	}
	if err := s.Process(ctx, event); err != nil || len(m.observations) != 1 {
		t.Fatal("durable duplicate was not suppressed")
	}
	g.pr.State = "closed"
	if err := s.Process(ctx, message("old-sync-after-close", "pull_request")); err != nil {
		t.Fatal(err)
	}
	if m.env.DesiredState != "Destroyed" {
		t.Fatal("late synchronize resurrected closed PR")
	}
	event.Payload = json.RawMessage(`{"changed":true}`)
	if err := s.Process(ctx, event); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatal("same delivery ID with different payload accepted")
	}
}
func TestForkAndConfigTrustRefused(t *testing.T) {
	for _, tc := range []string{"fork", "broaden TTL", "broaden visibility", "invalid config"} {
		t.Run(tc, func(t *testing.T) {
			s, m, g := setup(t)
			switch tc {
			case "fork":
				g.pr.Head.Repo.ID = 99
			case "broaden TTL":
				g.raw = []byte(strings.ReplaceAll(string(g.raw), "ttl: 2h", "ttl: 3h"))
			case "broaden visibility":
				g.raw = []byte(strings.ReplaceAll(string(g.raw), "visibility: private", "visibility: public"))
			case "invalid config":
				g.raw = []byte("version: 1\nprivileged: true\n")
			}
			if err := s.Process(context.Background(), message("refused", "pull_request")); err != nil {
				t.Fatal(err)
			}
			if len(m.observations) != 1 || m.observations[0].State != "refused" || m.observations[0].Message == "" || m.env.ID != "" {
				t.Fatal("unsafe preview was accepted or silent")
			}
		})
	}
}
func TestCommandsUseCurrentCommentAndPermission(t *testing.T) {
	for _, tc := range []struct {
		name, body, permission string
		action                 bool
	}{
		{"payload owner ignored", "/heimdall delete", "read", false}, {"triage denied", "/heimdall delete", "triage", false}, {"maintainer delete", "/heimdall delete", "maintain", true}, {"admin reset", "/heimdall reset", "admin", true}, {"writer retry", "/heimdall retry", "write", true}, {"writer extension", "/heimdall extend 2h", "write", true}, {"extra args", "/heimdall delete all", "write", false}, {"unknown command", "/heimdall destroy", "write", false}, {"negative extension", "/heimdall extend -1h", "write", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, g := setup(t)
			_, _ = s.observe(context.Background(), principal("tenant", "test"), m.repo, 101, "", "")
			g.comment.ID = 11
			g.comment.User.Login = "current-commenter"
			g.comment.Body = tc.body
			g.permission = tc.permission
			if err := s.Process(context.Background(), message("command-1", "issue_comment")); err != nil {
				t.Fatal(err)
			}
			if (len(m.actions) > 0) != tc.action {
				t.Fatalf("wrong authorization: actions %d responses %v", len(m.actions), m.commands)
			}
			if !tc.action && len(m.commands) != 1 {
				t.Fatal("denial was not audited/responded")
			}
		})
	}
}
func TestApprovalRequiresCurrentFullSHA(t *testing.T) {
	s, m, g := setup(t)
	g.baseline = nil
	g.comment.ID = 11
	g.comment.User.Login = "maintainer"
	g.comment.Body = "/heimdall approve " + strings.Repeat("f", 40)
	if err := s.Process(context.Background(), message("approve-old", "issue_comment")); err != nil {
		t.Fatal(err)
	}
	if m.pr.ApprovedSHA != "" {
		t.Fatal("approval was not bound to current head")
	}
	g.comment.Body = "/heimdall approve " + testHead
	if err := s.Process(context.Background(), message("approve-current", "issue_comment")); err != nil {
		t.Fatal(err)
	}
	if m.pr.ApprovedSHA != testHead {
		t.Fatal("current reviewed onboarding not approved")
	}
}
func buildRequest(t *testing.T, s *Service, input gen.BuildResult) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/github/build", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer signed-test-oidc")
	w := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(w, r)
	return w
}
func TestBuildPolicyAndApproval(t *testing.T) {
	for _, tc := range []string{"valid", "foreign registry", "tag bundle", "foreign image", "missing workload", "wrong PR", "missing baseline approval", "seed unattested"} {
		t.Run(tc, func(t *testing.T) {
			s, m, g := setup(t)
			input := gen.BuildResult{Bundle: testBundle, Commit: testHead, Images: map[string]string{"api": testImage}, PullRequest: 101, RepositoryID: 7}
			switch tc {
			case "foreign registry":
				input.Bundle = "evil.test/bundle@sha256:" + strings.Repeat("e", 64)
			case "tag bundle":
				input.Bundle = "registry.test/team/bundle:latest"
			case "foreign image":
				input.Images["api"] = "evil.test/api@sha256:" + strings.Repeat("d", 64)
			case "missing workload":
				input.Images = map[string]string{}
			case "wrong PR":
				input.PullRequest = 102
			case "missing baseline approval":
				g.baseline = nil
			case "seed unattested":
				g.raw = append(g.raw, []byte("dependencies:\n  postgres:\n    seed: data/seed.sql\n")...)
				g.baseline = g.raw
			}
			w := buildRequest(t, s, input)
			if tc == "valid" {
				if w.Code != 202 || len(m.builds) != 1 {
					t.Fatalf("valid build failed %d %s", w.Code, w.Body.String())
				}
				var spec v1.PreviewEnvironmentSpec
				if json.Unmarshal(m.builds[0].Spec, &spec) != nil || spec.Config.SHA256 != bundle.ConfigDigest(g.raw) || spec.Config.Baseline != string(g.baseline) || spec.Config.Bundle != testBundle {
					t.Fatal("config bytes not bound to trusted bundle projection")
				}
			} else if w.Code < 400 || len(m.builds) != 0 {
				t.Fatalf("unsafe build accepted %d", w.Code)
			}
		})
	}
}

func TestCanonicalWebhookTraceReachesBuildProjection(t *testing.T) {
	s, m, _ := setup(t)
	parent := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	ctx := tracecontext.Extract(context.Background(), parent)
	if _, err := s.observe(ctx, principal("tenant", "test"), m.repo, 101, "", ""); err != nil {
		t.Fatal(err)
	}
	input := gen.BuildResult{Bundle: testBundle, Commit: testHead, Images: map[string]string{"api": testImage}, PullRequest: 101, RepositoryID: 7}
	w := buildRequest(t, s, input)
	if w.Code != 202 || len(m.builds) != 1 {
		t.Fatalf("build failed: %d %s", w.Code, w.Body.String())
	}
	var spec v1.PreviewEnvironmentSpec
	if err := json.Unmarshal(m.builds[0].Spec, &spec); err != nil || spec.TraceParent != parent {
		t.Fatalf("canonical webhook trace was replaced by callback: %v %q", err, spec.TraceParent)
	}
}
func TestOutboxRecoversCrashAndRejectsForgedMarkers(t *testing.T) {
	s, m, g := setup(t)
	_, _ = s.observe(context.Background(), principal("tenant", "test"), m.repo, 101, "", "")
	m.env.BuildState = "ready"
	m.env.Phase = "Ready"
	m.env.Status = json.RawMessage(`{"urls":[{"service":"api","url":"https://pr101.preview.test"}],"diagnoses":[]}`)
	forged := githubapp.IssueComment{ID: 8, Body: noticeMarker(m.repo, 101)}
	forged.User.Login = "attacker"
	g.comments = []githubapp.IssueComment{forged}
	g.checkFailure = true
	item := domain.Outbox{ID: 1, EnvironmentID: m.env.ID, Generation: 1, Kind: "github.preview", LeaseToken: "lease"}
	p := principal("tenant", "outbox")
	if err := s.deliver(context.Background(), p, item); err == nil {
		t.Fatal("simulated crash not returned")
	}
	if m.delivery.CommentID != 0 {
		t.Fatal("failed transaction recorded partial delivery")
	}
	if err := s.deliver(context.Background(), p, item); err != nil {
		t.Fatal(err)
	}
	if g.commentCreates != 1 || g.commentEdits != 1 || g.checkCreates != 1 {
		t.Fatalf("crash duplicated GitHub resources: %d creates %d edits %d checks", g.commentCreates, g.commentEdits, g.checkCreates)
	}
	if g.comments[0].Body != noticeMarker(m.repo, 101) {
		t.Fatal("author's forged marker edited")
	}
	m.env.Generation = 2
	before := g.commentEdits
	if err := s.deliver(context.Background(), p, item); err != nil {
		t.Fatal(err)
	}
	if g.commentEdits != before {
		t.Fatal("stale generation edited comment")
	}
}
func TestPresentationNeverIncludesRawLogs(t *testing.T) {
	_, m, _ := setup(t)
	m.env = domain.Environment{ID: "env", Commit: testHead, Generation: 2, Phase: "Failed", BuildState: "ready", Status: json.RawMessage(`{"logs":"secret-value","diagnoses":[{"code":"MIGRATION_FAILED","summary":"secret-value","suggestion":"secret-value"}]}`)}
	body, check := Presentation(m.repo, m.env)
	if strings.Contains(body, "secret-value") || check.Conclusion != "failure" || !strings.Contains(body, "MIGRATION_FAILED") {
		t.Fatal("unsafe or incorrect presentation")
	}
}

func TestNarrowPRFitsTightenedPolicyEvenWhenBaselineExceedsIt(t *testing.T) {
	for _, cpu := range []string{"200m", "300m"} {
		t.Run(cpu, func(t *testing.T) {
			s, m, g := setup(t)
			m.policy.MaxContainerCPUMilli = 250
			resources := func(raw []byte, cpu string) []byte {
				return []byte(strings.ReplaceAll(string(raw), "    port: 8080", "    port: 8080\n    resources:\n      cpu: "+cpu+"\n      memory: 128Mi"))
			}
			g.baseline = resources(g.baseline, "500m")
			g.raw = resources(g.raw, cpu)
			w := buildRequest(t, s, gen.BuildResult{Bundle: testBundle, Commit: testHead, Images: map[string]string{"api": testImage}, PullRequest: 101, RepositoryID: 7})
			if cpu == "200m" && w.Code != 202 {
				t.Fatalf("valid narrowing rejected: %d %s", w.Code, w.Body.String())
			}
			if cpu == "300m" && w.Code < 400 {
				t.Fatal("PR exceeding current tenant policy accepted")
			}
		})
	}
}

var _ http.Handler = (&Service{}).BuildHandler()
