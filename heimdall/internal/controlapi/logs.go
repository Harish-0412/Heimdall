package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/redact"
)

// Log tails are transient and small. Neither the request's response bytes nor
// full application logs enter PostgreSQL, the audit log, or the outbox.
// Restarting an API instance expires requests; clients can request a new tail.
// LogRecord is a tenant, cluster and user scoped transient request or response.
type LogRecord struct {
	TenantID  string         `json:"tenantID"`
	ClusterID string         `json:"clusterID"`
	ActorID   string         `json:"actorID"`
	Request   gen.LogRequest `json:"request"`
}

type logRecord = LogRecord

func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request, action, id string, generation int64) bool {
	meta, _ := json.Marshal(map[string]any{"requestID": id, "generation": generation})
	if err := s.repo.AppendAudit(r.Context(), principal(r), action, id, meta); err != nil {
		s.err(w, r, err)
		return false
	}
	return true
}

func (s *Server) RequestLogs(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "admin", "member") {
		return
	}
	var in gen.LogInput
	if !s.decode(w, r, &in) {
		return
	}
	tail := 100
	if in.Tail != nil {
		tail = *in.Tail
	}
	if tail < 1 || tail > 200 || !dnsID.MatchString(in.Workload) {
		s.fail(w, r, 400, "api.invalid_request", "Workload must be a DNS name and tail must be 1 through 200")
		return
	}
	p := principal(r)
	e, err := s.repo.GetEnvironment(r.Context(), p, id)
	if err != nil {
		s.err(w, r, err)
		return
	}
	var spec struct {
		Config struct {
			Inline string `json:"inline"`
		} `json:"config"`
	}
	if json.Unmarshal(e.Spec, &spec) != nil {
		s.fail(w, r, 409, "state.unavailable", "The environment has no deployed configuration")
		return
	}
	policy, err := s.repo.GetPolicy(r.Context(), p)
	if err != nil {
		s.err(w, r, err)
		return
	}
	cfg, _ := config.Load(strings.NewReader(spec.Config.Inline), policy)
	found := false
	if cfg != nil {
		_, service := cfg.Services[in.Workload]
		_, worker := cfg.Workers[in.Workload]
		found = service || worker
	}
	if !found {
		s.fail(w, r, 400, "api.invalid_request", "Logs can only be requested for a declared application workload")
		return
	}
	request := gen.LogRequest{Id: randomID(), EnvironmentID: e.ID, Generation: e.Generation, Workload: in.Workload, Tail: tail, State: gen.LogRequestStatePending, ExpiresAt: time.Now().Add(s.opts.LogTTL)}
	if !s.auditLogs(w, r, "logs.request", request.Id, e.Generation) {
		return
	}
	err = s.logBroker.Create(r.Context(), logRecord{TenantID: p.TenantID, ClusterID: e.ClusterID, ActorID: p.ActorID, Request: request}, s.opts.LogTTL)
	if errors.Is(err, ErrLogLimit) {
		s.fail(w, r, 429, "api.rate_limited", "Too many active log requests; wait for earlier requests to expire")
		return
	}
	if err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 202, request)
}

func (s *Server) GetLogs(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "admin", "member") {
		return
	}
	p := principal(r)
	v, err := s.logBroker.Get(r.Context(), id)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.err(w, r, err)
		return
	}
	if err != nil || v.TenantID != p.TenantID || (v.ActorID != p.ActorID && p.Role != "admin") {
		s.err(w, r, domain.ErrNotFound)
		return
	}
	e, err := s.repo.GetEnvironment(r.Context(), p, v.Request.EnvironmentID)
	if err != nil {
		s.err(w, r, err)
		return
	}
	if e.Generation != v.Request.Generation {
		s.err(w, r, domain.ErrStaleGeneration)
		return
	}
	if v.Request.State != gen.LogRequestStatePending && !s.auditLogs(w, r, "logs.read", id, e.Generation) {
		return
	}
	s.json(w, 200, v.Request)
}

func (s *Server) PendingLogs(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "agent") {
		return
	}
	p := principal(r)
	pending, err := s.logBroker.Pending(r.Context(), p.TenantID, p.ClusterID)
	if err != nil {
		s.err(w, r, err)
		return
	}
	out := []gen.LogRequest{}
	for _, record := range pending {
		v := record.Request
		if record.TenantID != p.TenantID || record.ClusterID != p.ClusterID {
			s.fail(w, r, 503, "api.invalid_logs", "Log-request scope could not be verified")
			return
		}
		e, err := s.repo.GetEnvironment(r.Context(), p, v.EnvironmentID)
		if err != nil {
			s.err(w, r, err)
			return
		}
		if e.Generation == v.Generation {
			out = append(out, v)
		}
	}
	s.json(w, 200, out)
}

func (s *Server) CompleteLogs(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allow(w, r, "agent") {
		return
	}
	var in gen.LogResult
	if !s.decode(w, r, &in) {
		return
	}
	if !bool(in.Redacted) || in.Generation < 1 || (in.Text != nil && len(*in.Text) > 16384) || (in.Error != nil && len(*in.Error) > 256) {
		s.fail(w, r, 400, "api.invalid_request", "Log responses must be redacted by known values and within 16 KiB")
		return
	}
	p := principal(r)
	v, err := s.logBroker.Get(r.Context(), id)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.err(w, r, err)
		return
	}
	if err != nil || v.TenantID != p.TenantID || v.ClusterID != p.ClusterID {
		s.err(w, r, domain.ErrNotFound)
		return
	}
	e, err := s.repo.GetEnvironment(r.Context(), p, v.Request.EnvironmentID)
	if err != nil {
		s.err(w, r, err)
		return
	}
	if in.Generation != v.Request.Generation || e.Generation != in.Generation {
		s.err(w, r, domain.ErrStaleGeneration)
		return
	}
	if v.Request.State != gen.LogRequestStatePending {
		s.json(w, 204, nil)
		return
	}
	if !s.auditLogs(w, r, "logs.response", id, in.Generation) {
		return
	}
	if in.Text != nil {
		text := bounded(redact.Patterns(*in.Text), 16384)
		v.Request.Text = &text
	}
	if in.Error != nil && *in.Error != "" {
		message := bounded(redact.Patterns(*in.Error), 256)
		v.Request.Error = &message
		v.Request.State = gen.LogRequestStateFailed
	} else {
		v.Request.State = gen.LogRequestStateCompleted
	}
	if err := s.logBroker.Complete(r.Context(), v); err != nil {
		s.err(w, r, err)
		return
	}
	s.json(w, 204, nil)
}
