// Package orchestrator projects verified GitHub intent into tenant-scoped
// PostgreSQL state. GitHub is always re-read; webhook payloads are hints.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
	"github.com/heimdall-dev/heimdall/internal/queue"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Store interface {
	ResolveInstallation(context.Context, int64) (string, error)
	ActiveTenants(context.Context) ([]string, error)
	RepositoryByGitHubID(context.Context, domain.Principal, int64) (domain.Repository, error)
	GetRepository(context.Context, domain.Principal, string) (domain.Repository, error)
	GetTenant(context.Context, domain.Principal) (domain.Tenant, error)
	GetPolicy(context.Context, domain.Principal) (config.Policy, error)
	IngestWebhook(context.Context, domain.Principal, domain.WebhookDelivery) (bool, error)
	ClaimWebhook(context.Context, domain.Principal, string) (domain.WebhookDelivery, error)
	CompleteWebhook(context.Context, domain.Principal, string, string) error
	RetryWebhook(context.Context, domain.Principal, string, string, string) error
	ObservePullRequest(context.Context, domain.Principal, domain.PullRequestObservation) (domain.Environment, error)
	GetPullRequest(context.Context, domain.Principal, string, int64) (domain.PullRequest, error)
	EnvironmentByPullRequest(context.Context, domain.Principal, string, int64) (domain.Environment, error)
	ActEnvironment(context.Context, domain.Principal, string, int64, domain.Action, string) (domain.Environment, error)
	ApprovePullRequest(context.Context, domain.Principal, string, int64, string, string, string) error
	CommitBuild(context.Context, domain.Principal, domain.BuildInput) (domain.Environment, error)
	RecordCommand(context.Context, domain.Principal, string, string, string, int64, int64, string, string, string) error
	ClaimOutbox(context.Context, domain.Principal, int) ([]domain.Outbox, error)
	FinishOutbox(context.Context, domain.Principal, int64, string, int64, domain.GitHubDelivery) error
	RetryOutbox(context.Context, domain.Principal, int64, string, time.Duration, string) error
	WithGitHubDelivery(context.Context, domain.Principal, string, func(domain.Environment, domain.GitHubDelivery) (domain.GitHubDelivery, error)) error
	WithGitHubNotice(context.Context, domain.Principal, string, int64, func(domain.Repository, domain.PullRequest, *domain.Environment) error) error
}

type GitHubAPI interface {
	Repository(context.Context, githubapp.RepoRef) (githubapp.Repository, error)
	PullRequest(context.Context, githubapp.RepoRef, int64) (githubapp.PullRequest, error)
	DefaultBranchSHA(context.Context, githubapp.RepoRef, string) (string, error)
	Config(context.Context, githubapp.RepoRef, string) ([]byte, error)
	Permission(context.Context, githubapp.RepoRef, string) (string, error)
	Comment(context.Context, githubapp.RepoRef, int64) (githubapp.IssueComment, error)
	VerifyBuild(context.Context, githubapp.RepoRef, githubapp.ActionsIdentity, string, string, string) (githubapp.PullRequest, error)
	BotLogin(context.Context) (string, error)
	ListComments(context.Context, githubapp.RepoRef, int64) ([]githubapp.IssueComment, error)
	WriteComment(context.Context, githubapp.RepoRef, int64, int64, string) (int64, error)
	Checks(context.Context, githubapp.RepoRef, string) ([]githubapp.Check, error)
	OwnCheck(githubapp.Check) bool
	WriteCheck(context.Context, githubapp.RepoRef, int64, githubapp.CheckUpdate) (int64, error)
}

type IdentityVerifier interface {
	Verify(context.Context, string) (githubapp.ActionsIdentity, error)
}

// SeedApprovalProvider returns only an operator-approved hash and attestation.
// Fixture bytes travel from the customer's registry directly to their agent.
type SeedApprovalProvider func(context.Context, domain.Principal, string, int64, string) (v1.DataApproval, error)
type Options struct {
	Store           Store
	GitHub          GitHubAPI
	OIDC            IdentityVerifier
	SeedApproval    SeedApprovalProvider
	Now             func() time.Time
	DeliveryTimeout time.Duration
}
type Service struct{ o Options }

func New(o Options) (*Service, error) {
	if o.Store == nil || o.GitHub == nil {
		return nil, errors.New("orchestrator requires store and GitHub App")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DeliveryTimeout <= 0 || o.DeliveryTimeout > 90*time.Second {
		o.DeliveryTimeout = 45 * time.Second
	}
	return &Service{o: o}, nil
}
func principal(tenant, actor string) domain.Principal {
	return domain.Principal{TenantID: tenant, ActorID: actor, Role: "system", Kind: "system"}
}
func ref(r domain.Repository) githubapp.RepoRef {
	return githubapp.RepoRef{InstallationID: r.InstallationID, RepositoryID: r.GitHubID, FullName: r.FullName}
}

// Process durably inserts before handling and completes intent plus inbox in
// one store transaction. An SQS replay after five minutes sees the same row.
func (s *Service) Process(ctx context.Context, m queue.Message) (err error) {
	if err = m.Validate(); err != nil {
		return err
	}
	ctx = tracecontext.Extract(ctx, m.TraceParent)
	ctx, span := otel.Tracer("heimdall.github").Start(ctx, "github.orchestrate")
	defer span.End()
	span.SetAttributes(attribute.Int64("github.repository.id", m.RepositoryID), attribute.Int64("github.pull_request", m.PullRequest), attribute.String("github.delivery_id", m.DeliveryID))
	tenant, err := s.o.Store.ResolveInstallation(ctx, m.InstallationID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	p := principal(tenant, "github:webhook")
	span.SetAttributes(attribute.String("tenant", tenant))
	repo, err := s.o.Store.RepositoryByGitHubID(ctx, p, m.RepositoryID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !repo.Enabled || repo.InstallationID != m.InstallationID {
		return nil
	}
	if _, err = s.o.Store.IngestWebhook(ctx, p, domain.WebhookDelivery{DeliveryID: m.DeliveryID, Event: m.Event, RepositoryID: repo.ID, PullRequest: m.PullRequest, Payload: m.Payload}); err != nil {
		return err
	}
	d, err := s.o.Store.ClaimWebhook(ctx, p, m.DeliveryID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = s.o.Store.RetryWebhook(context.WithoutCancel(ctx), p, d.DeliveryID, d.LeaseToken, "github processing failed")
		}
	}()
	if m.Event == "issue_comment" {
		return s.command(ctx, p, repo, m, d)
	}
	_, err = s.observe(ctx, p, repo, m.PullRequest, d.DeliveryID, d.LeaseToken)
	return err
}

type trusted struct {
	PR            githubapp.PullRequest
	Config        *config.Config
	Raw, Baseline []byte
	BaseSHA       string
	NeedsApproval bool
	Problem       string
}

func (s *Service) trusted(ctx context.Context, p domain.Principal, r domain.Repository, n int64) (trusted, error) {
	var out trusted
	pr, err := s.o.GitHub.PullRequest(ctx, ref(r), n)
	if err != nil {
		return out, err
	}
	out.PR = pr
	if pr.State != "open" {
		return out, nil
	}
	if pr.Fork(r.GitHubID) {
		out.Problem = "Fork pull requests are disabled. No customer credentials or preview resources were issued."
		return out, nil
	}
	policy, err := s.o.Store.GetPolicy(ctx, p)
	if err != nil {
		return out, err
	}
	out.Raw, err = s.o.GitHub.Config(ctx, ref(r), pr.Head.SHA)
	if errors.Is(err, githubapp.ErrNotFound) {
		out.Problem = "Add heimdall.yaml and merge the onboarding files to the default branch."
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if len(out.Raw) > bundle.MaxConfigBytes {
		out.Problem = "heimdall.yaml exceeds 256 KiB."
		return out, nil
	}
	out.Config, _ = config.Load(bytes.NewReader(out.Raw), policy)
	if out.Config == nil {
		out.Problem = "heimdall.yaml does not satisfy the tenant policy. Run heimdall validate with the tenant policy to review the errors."
		return out, nil
	}
	repository, err := s.o.GitHub.Repository(ctx, ref(r))
	if err != nil {
		return out, err
	}
	out.BaseSHA, err = s.o.GitHub.DefaultBranchSHA(ctx, ref(r), repository.DefaultBranch)
	if err != nil {
		return out, err
	}
	out.Baseline, err = s.o.GitHub.Config(ctx, ref(r), out.BaseSHA)
	if errors.Is(err, githubapp.ErrNotFound) {
		out.NeedsApproval = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	baseline, _ := config.Load(bytes.NewReader(out.Baseline), config.BaselinePolicy())
	if baseline == nil {
		out.Problem = "The default-branch configuration is invalid and must be fixed before deploying previews."
		return out, nil
	}
	if len(config.CompareToBaseline(baseline, out.Config)) > 0 {
		out.Problem = "The PR configuration loosens the reviewed default-branch configuration. Merge the policy change to the default branch first."
	}
	return out, nil
}
func (s *Service) observe(ctx context.Context, p domain.Principal, r domain.Repository, n int64, delivery, lease string) (domain.Environment, error) {
	previous, err := s.o.Store.GetPullRequest(ctx, p, r.ID, n)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Environment{}, err
	}
	expectedVersion := int64(0)
	if err == nil {
		expectedVersion = previous.Version
	}
	t, err := s.trusted(ctx, p, r, n)
	if err != nil {
		return domain.Environment{}, err
	}
	state := t.PR.State
	if state != "open" {
		state = "closed"
	}
	if t.Problem != "" {
		state = "refused"
	}
	message := t.Problem
	if t.NeedsApproval {
		message = "No default-branch heimdall.yaml exists. A repository maintainer must review this exact commit and comment `/heimdall approve " + t.PR.Head.SHA + "` before the first deployment."
	}
	// Reject an observation whose API fetch raced a push. The next queue
	// delivery or build callback will retry from the new canonical head.
	latest, err := s.o.GitHub.PullRequest(ctx, ref(r), n)
	if err != nil {
		return domain.Environment{}, err
	}
	if latest.Head.SHA != t.PR.Head.SHA || latest.State != t.PR.State {
		return domain.Environment{}, domain.ErrConflict
	}
	return s.o.Store.ObservePullRequest(ctx, p, domain.PullRequestObservation{RepositoryID: r.ID, Number: n, HeadSHA: t.PR.Head.SHA, BaseSHA: t.BaseSHA, State: state, Owner: t.PR.User.Login, IsFork: t.PR.Fork(r.GitHubID), NeedsApproval: t.NeedsApproval, ConfigDigest: bundle.ConfigDigest(t.Raw), BaselineDigest: bundle.ConfigDigest(t.Baseline), DeliveryID: delivery, LeaseToken: lease, Message: message, UpdatedAt: t.PR.UpdatedAt, ExpectedVersion: expectedVersion, TraceParent: tracecontext.Capture(ctx)})
}

func (s *Service) command(ctx context.Context, p domain.Principal, r domain.Repository, m queue.Message, d domain.WebhookDelivery) error {
	var payload struct {
		Comment struct {
			ID int64 `json:"id"`
		} `json:"comment"`
	}
	if json.Unmarshal(m.Payload, &payload) != nil || payload.Comment.ID <= 0 {
		return s.o.Store.CompleteWebhook(ctx, p, d.DeliveryID, d.LeaseToken)
	}
	// The current comment and current permission are read from GitHub. Payload
	// author_association, login and body never grant permission.
	c, err := s.o.GitHub.Comment(ctx, ref(r), payload.Comment.ID)
	if errors.Is(err, githubapp.ErrNotFound) {
		return s.o.Store.CompleteWebhook(ctx, p, d.DeliveryID, d.LeaseToken)
	}
	if err != nil {
		return err
	}
	fields := strings.Fields(strings.TrimSpace(c.Body))
	if len(fields) == 0 || fields[0] != "/heimdall" {
		return s.o.Store.CompleteWebhook(ctx, p, d.DeliveryID, d.LeaseToken)
	}
	permission, err := s.o.GitHub.Permission(ctx, ref(r), c.User.Login)
	if err != nil {
		return err
	}
	p.ActorID = "github:" + c.User.Login
	reply := func(command, outcome, message string) error {
		return s.o.Store.RecordCommand(ctx, p, d.DeliveryID, d.LeaseToken, r.ID, m.PullRequest, c.ID, command, outcome, message)
	}
	if !githubapp.CanMaintain(permission) {
		return reply("heimdall", "denied", "This command requires current write, maintain, or admin permission on this repository.")
	}
	if len(fields) < 2 {
		return reply("heimdall", "invalid", "Use `/heimdall reset`, `/heimdall retry`, `/heimdall extend 2h`, or `/heimdall delete`.")
	}
	command := fields[1]
	pr, err := s.o.GitHub.PullRequest(ctx, ref(r), m.PullRequest)
	if err != nil {
		return err
	}
	if pr.State != "open" || pr.Fork(r.GitHubID) {
		return reply(command, "denied", "This PR is closed or is from a fork; preview commands are unavailable.")
	}
	if command == "approve" {
		if len(fields) != 3 || fields[2] != pr.Head.SHA {
			return reply(command, "invalid", "Approval must name the full current PR head SHA: `/heimdall approve "+pr.Head.SHA+"`.")
		}
		if _, err = s.observe(ctx, p, r, m.PullRequest, "", ""); err != nil {
			return err
		}
		if err = s.o.Store.ApprovePullRequest(ctx, p, r.ID, m.PullRequest, pr.Head.SHA, d.DeliveryID, d.LeaseToken); errors.Is(err, domain.ErrInvalidTransition) {
			return reply(command, "denied", "Approval is only used when the default-branch configuration is missing.")
		}
		return err
	}
	if !slices.Contains([]string{"reset", "retry", "extend", "delete"}, command) {
		return reply(command, "invalid", "Unknown Heimdall command. Use reset, retry, extend, or delete.")
	}
	e, err := s.o.Store.EnvironmentByPullRequest(ctx, p, r.ID, m.PullRequest)
	if errors.Is(err, domain.ErrNotFound) {
		return reply(command, "unavailable", "This PR has no preview environment yet.")
	}
	if err != nil {
		return err
	}
	action := domain.Action{Kind: command, DeliveryID: d.DeliveryID, LeaseToken: d.LeaseToken}
	if command == "extend" {
		if len(fields) != 3 {
			return reply(command, "invalid", "Specify the extension, for example `/heimdall extend 2h`.")
		}
		duration, err := config.ParseDuration(fields[2])
		if err != nil {
			return reply(command, "invalid", "The extension must be a positive duration, for example 2h or 1d.")
		}
		expiry := e.ExpiresAt.Add(duration)
		action.ExpiresAt = &expiry
	} else if len(fields) != 2 {
		return reply(command, "invalid", "This command does not accept extra arguments.")
	}
	_, err = s.o.Store.ActEnvironment(ctx, p, e.ID, e.Version, action, "github-command:"+d.DeliveryID)
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrForbidden) {
		return reply(command, "denied", "The preview's current state or tenant policy does not permit this command.")
	}
	return err
}

var digestRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func allowedReference(reference string, p config.Policy) bool {
	if !digestRef.MatchString(reference) || strings.Contains(reference, "..") {
		return false
	}
	for _, prefix := range p.AllowedRegistries {
		if strings.HasPrefix(reference, prefix) {
			return true
		}
	}
	return false
}

func (s *Service) BuildHandler() http.Handler { return http.HandlerFunc(s.build) }
func buildError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
func (s *Service) build(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		buildError(w, 405, "github.method", "POST required")
		return
	}
	if s.o.OIDC == nil {
		buildError(w, 503, "github.unavailable", "Actions authentication is not configured")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || token == "" {
		buildError(w, 401, "github.oidc", "GitHub Actions OIDC token required")
		return
	}
	id, err := s.o.OIDC.Verify(r.Context(), token)
	if err != nil {
		buildError(w, 401, "github.oidc", "Actions identity rejected")
		return
	}
	var input gen.BuildResult
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		buildError(w, 400, "github.build", "Invalid build notification")
		return
	}
	if err = controlapi.StrictJSON(body, &input); err != nil {
		buildError(w, 400, "github.build", "Invalid build notification")
		return
	}
	if input.RepositoryID != id.RepositoryID || input.PullRequest != id.PullRequest {
		buildError(w, 403, "github.identity", "Build identity does not match its signed workflow")
		return
	}
	// Resolve the repository only inside each authenticated tenant context.
	tenants, err := s.o.Store.ActiveTenants(r.Context())
	if err != nil {
		buildError(w, 503, "github.store", "Control plane unavailable")
		return
	}
	var repo domain.Repository
	var p domain.Principal
	found := false
	for _, tenant := range tenants {
		candidate := principal(tenant, "github:actions:"+id.RunID)
		repo, err = s.o.Store.RepositoryByGitHubID(r.Context(), candidate, id.RepositoryID)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			buildError(w, 503, "github.store", "Control plane unavailable")
			return
		}
		p = candidate
		found = true
		break
	}
	if !found || !repo.Enabled {
		buildError(w, 403, "github.repository", "Repository is not enabled")
		return
	}
	_, err = s.o.GitHub.VerifyBuild(r.Context(), ref(repo), id, input.Commit, repo.TrustedWorkflowRef, repo.TrustedWorkflowSHA)
	if err != nil {
		buildError(w, 403, "github.build_identity", "Build is stale, closed, from a fork, or not from the trusted workflow")
		return
	}
	t, err := s.trusted(r.Context(), p, repo, input.PullRequest)
	if err != nil {
		buildError(w, 503, "github.trust", "Configuration could not be verified")
		return
	}
	if t.PR.Head.SHA != input.Commit || t.PR.State != "open" || t.Problem != "" {
		buildError(w, 403, "github.trust", "Current configuration or PR state does not permit this build")
		return
	}
	e, err := s.observe(r.Context(), p, repo, input.PullRequest, "", "")
	if err != nil {
		buildError(w, 409, "github.conflict", "PR changed; retry with its current head")
		return
	}
	storedPR, err := s.o.Store.GetPullRequest(r.Context(), p, repo.ID, input.PullRequest)
	if err != nil {
		buildError(w, 503, "github.store", "Control plane unavailable")
		return
	}
	if t.NeedsApproval && storedPR.ApprovedSHA != input.Commit {
		buildError(w, 403, "github.approval", "Current commit requires maintainer approval")
		return
	}
	policy, err := s.o.Store.GetPolicy(r.Context(), p)
	if err != nil {
		buildError(w, 503, "github.store", "Tenant policy unavailable")
		return
	}
	// Policy may have tightened during the GitHub lookups. Re-evaluate every
	// trust layer and bind this exact policy to the committing transaction.
	currentConfig, _ := config.Load(bytes.NewReader(t.Raw), policy)
	if currentConfig == nil {
		buildError(w, 403, "github.policy", "Current tenant policy does not permit this configuration")
		return
	}
	if len(t.Baseline) > 0 {
		currentBaseline, _ := config.Load(bytes.NewReader(t.Baseline), config.BaselinePolicy())
		if currentBaseline == nil || len(config.CompareToBaseline(currentBaseline, currentConfig)) > 0 {
			buildError(w, 403, "github.trust", "Current baseline and tenant policy do not permit this configuration")
			return
		}
	}
	t.Config = currentConfig
	policyJSON, _ := json.Marshal(policy)
	if !allowedReference(input.Bundle, policy) {
		buildError(w, 403, "github.registry", "Bundle must be pinned by SHA-256 in an allowed customer registry")
		return
	}
	wanted := map[string]string{}
	for name, svc := range t.Config.Services {
		wanted[name] = svc.Image
	}
	for name, worker := range t.Config.Workers {
		wanted[name] = worker.Image
	}
	if len(input.Images) != len(wanted) {
		buildError(w, 400, "github.images", "One image digest is required for every service and worker")
		return
	}
	for name, original := range wanted {
		image, ok := input.Images[name]
		if !ok || !allowedReference(image, policy) || (original != "" && image != original) {
			buildError(w, 403, "github.images", "Image digest is missing, outside registry policy, or changed from a declared prebuilt image")
			return
		}
	}
	tenant, err := s.o.Store.GetTenant(r.Context(), p)
	if err != nil {
		buildError(w, 503, "github.store", "Tenant unavailable")
		return
	}
	suffix := strings.TrimPrefix(e.ID, "env-")
	suffix = strings.ReplaceAll(suffix, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if len(e.Spec) > 2 {
		var prior v1.PreviewEnvironmentSpec
		if json.Unmarshal(e.Spec, &prior) == nil && prior.URLSuffix != "" {
			suffix = prior.URLSuffix
		}
	}
	expires := s.o.Now().UTC().Add(t.Config.Preview.TTL.Std())
	spec := v1.PreviewEnvironmentSpec{TraceParent: storedPR.TraceParent, Tenant: tenant.Slug, Repository: repo.FullName, PullRequest: input.PullRequest, Commit: input.Commit, Generation: e.Generation, EnvironmentID: e.ID, Owner: t.PR.User.Login, URLSuffix: suffix, ExpiresAt: metav1.NewTime(expires), DesiredState: v1.DesiredRunning, ResetNonce: e.ResetNonce, Images: input.Images, Config: v1.ConfigSource{Inline: string(t.Raw), SHA256: bundle.ConfigDigest(t.Raw), Bundle: input.Bundle, Baseline: string(t.Baseline), BaselineSHA256: bundle.ConfigDigest(t.Baseline)}}
	if t.NeedsApproval {
		spec.Config.ApprovedBy = "github-maintainer:" + input.Commit
	}
	if t.Config.Dependencies.Postgres != nil && t.Config.Dependencies.Postgres.Seed != "" {
		if s.o.SeedApproval == nil {
			buildError(w, 403, "github.seed_approval", "SQL import needs a content-bound operator attestation")
			return
		}
		approval, err := s.o.SeedApproval(r.Context(), p, repo.ID, input.PullRequest, spec.Config.SHA256)
		if err != nil || !approval.Sanitised || len(approval.SHA256) != 64 || approval.ApprovedBy == "" || approval.Reason == "" {
			buildError(w, 403, "github.seed_approval", "SQL import needs a content-bound operator attestation")
			return
		}
		spec.Data = &v1.DataSource{ConfigMap: "heimdall-data-" + e.ID + "-" + approval.SHA256[:12], Key: "seed.sql", Approval: approval}
	}
	specJSON, _ := json.Marshal(spec)
	e, err = s.o.Store.CommitBuild(r.Context(), p, domain.BuildInput{RepositoryID: repo.ID, Number: input.PullRequest, HeadSHA: input.Commit, Bundle: input.Bundle, Images: input.Images, ConfigDigest: spec.Config.SHA256, BaselineDigest: spec.Config.BaselineSHA256, RunID: id.RunID, RunAttempt: id.RunAttempt, WorkflowRef: id.WorkflowRef, Spec: specJSON, ExpiresAt: expires, PolicySHA256: bundle.ConfigDigest(policyJSON)})
	if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrStaleGeneration) {
		buildError(w, 409, "github.conflict", "Build was superseded; retry the current head")
		return
	}
	if err != nil {
		buildError(w, 403, "github.build", "Build was not accepted")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"environmentID": e.ID, "generation": e.Generation, "version": e.Version})
}

// RunQueue consumes one item at a time so a failed FIFO item cannot be bypassed
// by a later item from the same group in the same receive batch.
func (s *Service) RunQueue(ctx context.Context, q *queue.SQS) error {
	for ctx.Err() == nil {
		batch, err := q.Receive(ctx, 1)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, item := range batch {
			job, cancel := context.WithTimeout(ctx, 90*time.Second)
			err = s.Process(job, item.Message)
			cancel()
			if err == nil {
				if err = q.Ack(ctx, item); err != nil {
					return err
				}
			} else {
				if err = q.Extend(ctx, item, 30); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

func noticeMarker(repo domain.Repository, n int64) string {
	return "<!-- heimdall-preview:v1:" + strconv.FormatInt(repo.GitHubID, 10) + ":" + strconv.FormatInt(n, 10) + " -->"
}
func checkExternal(e domain.Environment) string {
	return fmt.Sprintf("heimdall:%s:%d", e.ID, e.Generation)
}
