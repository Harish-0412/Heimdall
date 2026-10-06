package controlapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/controller"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

func (s *Server) CreateEnvironment(w http.ResponseWriter, r *http.Request, params gen.CreateEnvironmentParams) {
	if !s.allow(w, r, "admin") {
		return
	}
	var in gen.CreateEnvironment
	if !s.decode(w, r, &in) {
		return
	}
	if !idemID.MatchString(params.IdempotencyKey) || !dnsID.MatchString(in.Name) || in.RepositoryID == "" || in.ClusterID == "" || len(in.Spec) > 384<<10 {
		s.fail(w, r, 400, "api.invalid_request", "A repository, cluster, DNS environment name, bounded spec and idempotency key are required")
		return
	}
	p := principal(r)
	repo, err := s.repo.GetRepository(r.Context(), p, in.RepositoryID)
	if err != nil {
		s.err(w, r, err)
		return
	}
	if !repo.Enabled || repo.ClusterID != in.ClusterID {
		s.fail(w, r, 400, "policy.repository", "The enabled repository must be assigned to this cluster")
		return
	}
	if _, err := s.repo.GetCluster(r.Context(), p, in.ClusterID); err != nil {
		s.err(w, r, err)
		return
	}
	policy, err := s.repo.GetPolicy(r.Context(), p)
	if err != nil {
		s.err(w, r, err)
		return
	}
	if err := ValidatePolicy(policy); err != nil {
		s.fail(w, r, 503, "policy.unavailable", "A valid administrator policy is required before deploying")
		return
	}
	var spec v1alpha1.PreviewEnvironmentSpec
	if StrictJSON(in.Spec, &spec) != nil {
		s.fail(w, r, 400, "config.invalid", "The preview spec has unknown or invalid fields")
		return
	}
	if spec.Tenant != p.TenantSlug || p.TenantSlug == "" || spec.Repository != repo.FullName || spec.EnvironmentID != in.Name || spec.Generation != 1 || spec.ResetNonce != 0 || (spec.DesiredState != "" && spec.DesiredState != v1alpha1.DesiredRunning) {
		s.fail(w, r, 400, "config.identity", "Spec identity must match the authenticated tenant, repository and name; initial generation must be 1")
		return
	}
	spec.DesiredState = v1alpha1.DesiredRunning
	// Raw manual input is an administrator-controlled override. Members use
	// the GitHub path, where baseline and approval come from canonical GitHub
	// state. Never accept a caller-supplied identity as approval evidence.
	if spec.Config.Baseline == "" {
		spec.Config.ApprovedBy = p.ActorID
	} else {
		spec.Config.ApprovedBy = ""
	}
	now := time.Now()
	if spec.ExpiresAt.Time.Before(now.Add(policy.MinTTL)) || spec.ExpiresAt.After(now.Add(policy.MaxTTL)) {
		s.fail(w, r, 400, "policy.ttl", "Expiry must remain within tenant TTL limits")
		return
	}
	b := controller.SpecBuilder{Policy: policy, Platform: render.Platform{BaseDomain: "preview.invalid"}}
	if _, err := b.Validate(r.Context(), &v1alpha1.PreviewEnvironment{Spec: spec}); err != nil {
		s.fail(w, r, 400, "config.invalid", redact.Patterns(err.Error()))
		return
	}
	data, _ := json.Marshal(spec)
	env, err := s.repo.CreateEnvironment(r.Context(), p, domain.CreateEnvironment{ID: spec.EnvironmentID, RepositoryID: in.RepositoryID, ClusterID: in.ClusterID, Name: in.Name, Owner: spec.Owner, Commit: spec.Commit, PullRequest: spec.PullRequest, Spec: data, ExpiresAt: spec.ExpiresAt.Time}, params.IdempotencyKey)
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 201, toEnvironment(env))
}

// ValidatePolicy rejects implicit local-mode allowances. Limits and lists must
// be explicit so missing tenant context can never become an unrestricted policy.
func ValidatePolicy(p config.Policy) error {
	if p.MaxServices < 0 || p.MaxServices > 16 || p.MaxWorkers < 0 || p.MaxWorkers > 16 || p.MaxServices+p.MaxWorkers == 0 || p.MaxServices+p.MaxWorkers > 16 || p.MaxContainerCPUMilli <= 0 || p.MaxContainerCPUMilli > 100000 || p.MaxContainerMemoryMi <= 0 || p.MaxContainerMemoryMi > 1<<20 || p.MaxTotalCPUMilli < p.MaxContainerCPUMilli || p.MaxTotalCPUMilli > 1000000 || p.MaxTotalMemoryMi < p.MaxContainerMemoryMi || p.MaxTotalMemoryMi > 1<<24 || p.MaxPostgresStorageMi <= 0 || p.MaxPostgresStorageMi > 1<<24 || p.MinMemoryRequestPercent < 1 || p.MinMemoryRequestPercent > 100 || p.MinTTL <= 0 || p.MaxTTL < p.MinTTL || p.MaxTTL > 30*24*time.Hour {
		return domain.ErrForbidden
	}
	if p.MaxVisibility != config.VisibilityPrivate && p.MaxVisibility != config.VisibilityOrg && p.MaxVisibility != config.VisibilityPublic {
		return domain.ErrForbidden
	}
	if p.AllowedSecrets == nil || p.AllowedRegistries == nil || len(p.AllowedSecrets) > 128 || len(p.AllowedRegistries) > 128 {
		return domain.ErrForbidden
	}
	for _, name := range p.AllowedSecrets {
		if len(name) > 253 || name == "" || strings.ContainsAny(name, "\r\n\x00") {
			return domain.ErrForbidden
		}
	}
	for _, prefix := range p.AllowedRegistries {
		if prefix == "" || len(prefix) > 253 || strings.ContainsAny(prefix, " \t\r\n@\x00") || !strings.HasSuffix(prefix, "/") {
			return domain.ErrForbidden
		}
	}
	return nil
}
func (s *Server) GetPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "admin", "member", "viewer", "agent") {
		return
	}
	p, err := s.repo.GetPolicy(r.Context(), principal(r))
	if err != nil {
		s.err(w, r, err)
		return
	}
	if ValidatePolicy(p) != nil {
		s.fail(w, r, 503, "policy.unavailable", "A valid administrator policy is required")
		return
	}
	s.json(w, 200, p)
}
func (s *Server) SetPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "admin") {
		return
	}
	var p config.Policy
	if !s.decode(w, r, &p) {
		return
	}
	if ValidatePolicy(p) != nil {
		s.fail(w, r, 400, "policy.invalid", "Policy needs bounded positive limits, TTL, visibility and explicit allowed secret and registry lists")
		return
	}
	if err := s.repo.SetPolicy(r.Context(), principal(r), p); err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 200, p)
}

func toCluster(c domain.Cluster) gen.Cluster {
	return gen.Cluster{Id: c.ID, TenantID: c.TenantID, Name: c.Name, Tier: c.Tier, LastHeartbeat: c.LastHeartbeat}
}
func (s *Server) ListClusters(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "admin", "member", "viewer") {
		return
	}
	rows, err := s.repo.ListClusters(r.Context(), principal(r))
	if err != nil {
		s.err(w, r, err)
		return
	}
	out := []gen.Cluster{}
	for _, c := range rows {
		out = append(out, toCluster(c))
	}
	s.json(w, 200, out)
}
func (s *Server) CreateCluster(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "admin") {
		return
	}
	var in gen.ClusterInput
	if !s.decode(w, r, &in) {
		return
	}
	if !dnsID.MatchString(in.Name) || !in.Tier.Valid() {
		s.fail(w, r, 400, "api.invalid_request", "A DNS cluster name and isolation tier 0 or 1 are required")
		return
	}
	c, err := s.repo.CreateCluster(r.Context(), principal(r), domain.Cluster{Name: in.Name, Tier: int(in.Tier)})
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 201, toCluster(c))
}
func (s *Server) EnrollCluster(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "admin") {
		return
	}
	if _, err := s.repo.GetCluster(r.Context(), principal(r), id); err != nil {
		s.err(w, r, err)
		return
	}
	c, err := s.repo.IssueCredential(r.Context(), principal(r), domain.CredentialInput{ActorID: "cluster:" + id, ClusterID: id, Role: "agent", Kind: "enrollment", TTL: 10 * time.Minute})
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 201, gen.Enrollment{ClusterID: id, Token: c.Token, ExpiresAt: c.ExpiresAt})
}
func (s *Server) RevokeCluster(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "admin") {
		return
	}
	if err := s.repo.RevokeClusterCredentials(r.Context(), principal(r), id); err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 204, nil)
}
func pairDTO(p domain.CredentialPair) gen.TokenPair {
	return gen.TokenPair{AccessToken: p.Access.Token, AccessExpiresAt: p.Access.ExpiresAt, RefreshToken: p.Refresh.Token, RefreshExpiresAt: p.Refresh.ExpiresAt, ClusterID: p.Access.ClusterID}
}
func (s *Server) RegisterAgent(w http.ResponseWriter, r *http.Request, params gen.RegisterAgentParams) {
	var in gen.AgentRegistration
	if !s.decode(w, r, &in) {
		return
	}
	token := bearer(r)
	if token == "" || !idemID.MatchString(params.IdempotencyKey) || in.ClusterID == "" || len(in.ClusterID) > 128 || in.Version == "" || len(in.Version) > 64 {
		s.fail(w, r, 400, "api.invalid_request", "An enrollment bearer, clusterID and agent version are required")
		return
	}
	pair, err := s.repo.ExchangeCredentialWithNonce(r.Context(), token, "enrollment", in.ClusterID, params.IdempotencyKey)
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 200, pairDTO(pair))
}
func (s *Server) RefreshAgent(w http.ResponseWriter, r *http.Request, params gen.RefreshAgentParams) {
	token := bearer(r)
	if token == "" {
		s.fail(w, r, 401, "auth.required", "A refresh bearer credential is required")
		return
	}
	if !idemID.MatchString(params.IdempotencyKey) {
		s.fail(w, r, 400, "api.invalid_request", "Persist an idempotency key before rotating the credential")
		return
	}
	pair, err := s.repo.ExchangeCredentialWithNonce(r.Context(), token, "refresh", "", params.IdempotencyKey)
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 200, pairDTO(pair))
}
func (s *Server) HeartbeatAgent(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "agent") {
		return
	}
	var in gen.Heartbeat
	if !s.decode(w, r, &in) {
		return
	}
	if in.Version == "" || len(in.Version) > 64 {
		s.fail(w, r, 400, "api.invalid_request", "A bounded agent version is required")
		return
	}
	if _, err := s.repo.Heartbeat(r.Context(), principal(r), in.Version); err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 204, nil)
}
func (s *Server) PullDesired(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "agent") {
		return
	}
	snap, err := s.repo.Snapshot(r.Context(), principal(r))
	if err != nil {
		s.err(w, r, err)
		return
	}
	if ValidatePolicy(snap.Policy) != nil {
		s.fail(w, r, 503, "policy.unavailable", "A valid tenant policy is required before synchronizing")
		return
	}
	p := principal(r)
	if snap.Revision < 1 || p.TenantSlug == "" {
		s.fail(w, r, 503, "api.invalid_snapshot", "Desired-state revision and tenant identity are required")
		return
	}
	out := gen.DesiredSnapshot{Revision: strconv.FormatInt(snap.Revision, 10), Environments: []gen.Environment{}, Policy: snap.Policy, TenantID: p.TenantID, TenantSlug: p.TenantSlug}
	for _, e := range snap.Environments {
		if e.ClusterID != principal(r).ClusterID || e.TenantID != principal(r).TenantID {
			s.fail(w, r, 503, "api.invalid_snapshot", "Desired-state scope could not be verified")
			return
		}
		out.Environments = append(out.Environments, toEnvironment(e))
	}
	s.json(w, 200, out)
}

func (s *Server) PushStatus(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "agent") {
		return
	}
	var in gen.StatusUpdate
	if !s.decode(w, r, &in) {
		return
	}
	if in.Generation < 1 || in.Version < 1 || !in.Phase.Valid() || !safeID.MatchString(in.EventID) || len(in.Status) > 64<<10 {
		s.fail(w, r, 400, "api.invalid_request", "A current generation, version, eventID, supported phase and bounded status are required")
		return
	}
	var st v1alpha1.PreviewEnvironmentStatus
	if StrictJSON(in.Status, &st) != nil || string(st.Phase) != string(in.Phase) || st.DeployedGeneration > in.Generation || st.CompletedResetNonce < 0 || st.ObservedGeneration < 0 || len(st.Steps) > 64 || len(st.Conditions) > 16 || len(st.URLs) > 16 || len(st.Diagnoses) > 5 {
		s.fail(w, r, 400, "api.invalid_status", "Runtime status is invalid or exceeds its bounds")
		return
	}
	for _, u := range st.URLs {
		parsed, err := url.Parse(u.URL)
		if len(u.URL) > 2048 || !dnsID.MatchString(u.Service) || err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			s.fail(w, r, 400, "api.invalid_status", "Runtime URLs exceed their bounds")
			return
		}
	}
	if st.Namespace != "" && !dnsID.MatchString(st.Namespace) {
		s.fail(w, r, 400, "api.invalid_status", "Runtime namespace must be a DNS name")
		return
	}
	if st.Operation != nil {
		op := st.Operation
		if op.Generation < 1 || op.Generation > in.Generation || op.ResetNonce < 0 || op.Attempts < 0 || (op.Type != v1alpha1.OperationApply && op.Type != v1alpha1.OperationReset && op.Type != v1alpha1.OperationDestroy) || (op.Result != v1alpha1.ResultQueued && op.Result != v1alpha1.ResultRunning && op.Result != v1alpha1.ResultSucceeded && op.Result != v1alpha1.ResultFailed && op.Result != v1alpha1.ResultCancelled && op.Result != v1alpha1.ResultInterrupted) {
			s.fail(w, r, 400, "api.invalid_status", "Runtime operation is invalid")
			return
		}
	}
	smokeRuns := []domain.SmokeRun{}
	for i := range st.Steps {
		if st.Steps[i].DurationSeconds < 0 || st.Steps[i].DurationSeconds > 24*60*60 {
			s.fail(w, r, 400, "api.invalid_status", "Runtime step duration is invalid")
			return
		}
		st.Steps[i].Name = bounded(st.Steps[i].Name, 253)
		st.Steps[i].Code = bounded(st.Steps[i].Code, 64)
		st.Steps[i].State = bounded(st.Steps[i].State, 64)
		if strings.HasPrefix(st.Steps[i].Name, "smoke/") && (st.Steps[i].State == "succeeded" || st.Steps[i].State == "failed") {
			smokeRuns = append(smokeRuns, domain.SmokeRun{Name: st.Steps[i].Name, Passed: st.Steps[i].State == "succeeded", DurationMS: st.Steps[i].DurationSeconds * 1000})
		}
	}
	for i := range st.Conditions {
		c := &st.Conditions[i]
		if c.ObservedGeneration < 0 || (c.Status != "True" && c.Status != "False" && c.Status != "Unknown") || c.Type == "" || len(c.Type) > 128 {
			s.fail(w, r, 400, "api.invalid_status", "Runtime condition is invalid")
			return
		}
		st.Conditions[i].Message = bounded(redact.Patterns(st.Conditions[i].Message), 1024)
		st.Conditions[i].Reason = bounded(st.Conditions[i].Reason, 128)
	}
	if st.LastError != nil {
		if st.LastError.Generation != in.Generation {
			s.fail(w, r, 400, "api.invalid_status", "Error generation must match the reported generation")
			return
		}
		st.LastError.Message = bounded(redact.Patterns(st.LastError.Message), 1024)
		st.LastError.Code = bounded(st.LastError.Code, 64)
		st.LastError.Step = bounded(st.LastError.Step, 253)
	}
	diagnoses := make([]domain.Diagnosis, 0, len(st.Diagnoses))
	for i := range st.Diagnoses {
		d := &st.Diagnoses[i]
		if len(d.Evidence) > 10 {
			s.fail(w, r, 400, "api.invalid_status", "Diagnosis evidence exceeds its bounds")
			return
		}
		d.Code = bounded(d.Code, 64)
		d.Subject = bounded(redact.Patterns(d.Subject), 253)
		d.Stage = bounded(d.Stage, 64)
		d.Summary = bounded(redact.Patterns(d.Summary), 1024)
		d.Suggestion = bounded(redact.Patterns(d.Suggestion), 1024)
		for j := range d.Evidence {
			d.Evidence[j] = bounded(redact.Patterns(d.Evidence[j]), 256)
		}
		diagnoses = append(diagnoses, domain.Diagnosis{Code: d.Code, Summary: d.Summary, Suggestion: d.Suggestion, Evidence: d.Evidence, Subject: d.Subject, Stage: d.Stage})
	}
	status, _ := json.Marshal(st)
	update := domain.StatusUpdate{Generation: in.Generation, Version: in.Version, Phase: string(in.Phase), Status: status, EventID: in.EventID, Diagnoses: diagnoses, SmokeRuns: smokeRuns}
	if st.LastError != nil {
		update.Step = st.LastError.Step
	}
	env, err := s.repo.ReportStatus(r.Context(), principal(r), id, update)
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 200, toEnvironment(env))
}
func bounded(v string, max int) string {
	if len(v) <= max {
		return v
	}
	v = v[:max]
	for len(v) > 0 && !json.Valid([]byte(strconv.Quote(v))) {
		v = v[:len(v)-1]
	}
	return strings.ToValidUTF8(v, "")
}
