package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/jackc/pgx/v5"
)

func idempotency(ctx context.Context, tx pgx.Tx, p domain.Principal, key string, request any) (*domain.Environment, error) {
	if key == "" {
		return nil, nil
	}
	if len(key) > 200 {
		return nil, errors.New("idempotency key exceeds 200 bytes")
	}
	hash := sha256.Sum256(raw(request))
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.idempotency_keys(tenant_id,actor_id,key,request_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.TenantID, p.ActorID, key, hash[:])
	if err != nil {
		return nil, err
	}
	var savedHash, response []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM heimdall.idempotency_keys WHERE tenant_id=$1 AND actor_id=$2 AND key=$3 FOR UPDATE`, p.TenantID, p.ActorID, key).Scan(&savedHash, &response)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(hash[:], savedHash) {
		return nil, domain.ErrIdempotencyConflict
	}
	if response == nil {
		return nil, nil
	}
	var e domain.Environment
	if err = json.Unmarshal(response, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
func saveIdempotency(ctx context.Context, tx pgx.Tx, p domain.Principal, key string, e domain.Environment) error {
	if key == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE heimdall.idempotency_keys SET response=$4 WHERE tenant_id=$1 AND actor_id=$2 AND key=$3`, p.TenantID, p.ActorID, key, raw(e))
	return err
}
func mutateSpec(e domain.Environment) json.RawMessage {
	var spec map[string]any
	if json.Unmarshal(e.Spec, &spec) != nil || spec == nil {
		spec = map[string]any{}
	}
	spec["generation"] = e.Generation
	spec["resetNonce"] = e.ResetNonce
	spec["desiredState"] = e.DesiredState
	spec["expiresAt"] = e.ExpiresAt.UTC().Format(time.RFC3339)
	return raw(spec)
}
func deployment(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.Environment) error {
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.deployments(id,tenant_id,environment_id,generation,commit_sha,spec) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewString(), p.TenantID, e.ID, e.Generation, e.Commit, e.Spec)
	return err
}
func capacity(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	var maximum, count int
	err := tx.QueryRow(ctx, `SELECT max_environments FROM heimdall.quotas WHERE tenant_id=$1 FOR UPDATE`, p.TenantID).Scan(&maximum)
	if err != nil {
		return translate(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM heimdall.environments WHERE desired_state='Running' AND phase<>'Destroyed'`).Scan(&count); err != nil {
		return err
	}
	if count >= maximum {
		return domain.ErrQuotaExceeded
	}
	return nil
}
func insertEnvironment(ctx context.Context, tx pgx.Tx, p domain.Principal, input domain.CreateEnvironment, buildState string) (domain.Environment, error) {
	if input.ID == "" {
		input.ID = "env-" + uuid.NewString()
	}
	if input.Name == "" {
		input.Name = input.ID
	}
	if input.ExpiresAt.IsZero() {
		input.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
	}
	if len(input.Spec) == 0 {
		input.Spec = raw(map[string]any{})
	}
	e, err := scanEnvironment(tx.QueryRow(ctx, `INSERT INTO heimdall.environments(id,tenant_id,repository_id,cluster_id,name,pull_request,owner,commit_sha,desired_state,phase,build_state,spec,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'Running','Pending',$9,$10,$11) RETURNING `+environmentColumns, input.ID, p.TenantID, input.RepositoryID, input.ClusterID, input.Name, input.PullRequest, input.Owner, input.Commit, buildState, input.Spec, input.ExpiresAt))
	return e, err
}
func (s *Store) CreateEnvironment(ctx context.Context, p domain.Principal, input domain.CreateEnvironment, key string) (domain.Environment, error) {
	if err := requireHuman(p); err != nil {
		return domain.Environment{}, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	replay, err := idempotency(ctx, tx, p, key, struct {
		Operation string
		Input     domain.CreateEnvironment
	}{"create", input})
	if err != nil {
		return domain.Environment{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	if err = capacity(ctx, tx, p); err != nil {
		return domain.Environment{}, err
	}
	e, err := insertEnvironment(ctx, tx, p, input, "ready")
	if err != nil {
		return e, err
	}
	e.Spec = mutateSpec(e)
	if _, err = tx.Exec(ctx, `UPDATE heimdall.environments SET spec=$2 WHERE id=$1`, e.ID, e.Spec); err != nil {
		return e, err
	}
	if err = deployment(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = event(ctx, tx, p, e, "environment.created", map[string]any{"phase": e.Phase}, ""); err != nil {
		return e, err
	}
	if err = audit(ctx, tx, p, "environment.create", e.ID, map[string]any{"generation": e.Generation}); err != nil {
		return e, err
	}
	if err = notify(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = revision(ctx, tx, p); err != nil {
		return e, err
	}
	if err = saveIdempotency(ctx, tx, p, key, e); err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func updateEnvironment(ctx context.Context, tx pgx.Tx, e domain.Environment, version int64) (domain.Environment, error) {
	return scanEnvironment(tx.QueryRow(ctx, `UPDATE heimdall.environments SET commit_sha=$2,generation=$3,reset_nonce=$4,desired_state=$5,phase=$6,build_state=$7,spec=$8,status=$9,expires_at=$10,version=version+1,updated_at=now() WHERE id=$1 AND version=$11 RETURNING `+environmentColumns, e.ID, e.Commit, e.Generation, e.ResetNonce, e.DesiredState, e.Phase, e.BuildState, e.Spec, e.Status, e.ExpiresAt, version))
}
func (s *Store) ActEnvironment(ctx context.Context, p domain.Principal, id string, expectedVersion int64, action domain.Action, key string) (domain.Environment, error) {
	if err := requireHuman(p); err != nil {
		return domain.Environment{}, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	replay, err := idempotency(ctx, tx, p, key, struct {
		Operation, ID string
		Version       int64
		Action        domain.Action
	}{"action", id, expectedVersion, action})
	if err != nil {
		return domain.Environment{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	e, err := getEnvironment(ctx, tx, id, true)
	if err != nil {
		return e, err
	}
	if e.Version != expectedVersion {
		return e, domain.ErrConflict
	}
	e, err = act(ctx, tx, p, e, action)
	if err != nil {
		return e, err
	}
	if err = completeDelivery(ctx, tx, p, action.DeliveryID, action.LeaseToken); err != nil {
		return e, err
	}
	if err = saveIdempotency(ctx, tx, p, key, e); err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func act(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.Environment, action domain.Action) (domain.Environment, error) {
	old := e.Version
	oldGeneration := e.Generation
	switch action.Kind {
	case "retry":
		if e.DesiredState != "Running" || e.Phase == "Destroyed" || e.Phase == "Destroying" || e.BuildState != "ready" {
			return e, domain.ErrInvalidTransition
		}
		e.Generation++
		e.Phase = "Pending"
		e.Status = raw(map[string]any{})
	case "reset":
		if e.DesiredState != "Running" || e.BuildState != "ready" || (e.Phase != "Ready" && e.Phase != "Failed" && e.Phase != "Degraded") {
			return e, domain.ErrInvalidTransition
		}
		e.Generation++
		e.ResetNonce++
		e.Phase = "Resetting"
		e.Status = raw(map[string]any{})
	case "delete":
		if e.DesiredState == "Destroyed" {
			return e, nil
		}
		e.Generation++
		e.DesiredState = "Destroyed"
		e.Phase = "Destroying"
		e.BuildState = "ready"
		e.Status = raw(map[string]any{})
	case "extend":
		if e.DesiredState != "Running" || action.ExpiresAt == nil || !action.ExpiresAt.After(e.ExpiresAt) {
			return e, domain.ErrInvalidTransition
		}
		policy, err := getPolicy(ctx, tx, p)
		if err != nil {
			return e, err
		}
		if action.ExpiresAt.After(time.Now().UTC().Add(policy.MaxTTL)) {
			return e, domain.ErrForbidden
		}
		e.ExpiresAt = action.ExpiresAt.UTC()
	default:
		return e, fmt.Errorf("%w: unknown action %q", domain.ErrInvalidTransition, action.Kind)
	}
	e.Spec = mutateSpec(e)
	var err error
	e, err = updateEnvironment(ctx, tx, e, old)
	if err != nil {
		return e, err
	}
	if e.Generation != oldGeneration {
		if err = deployment(ctx, tx, p, e); err != nil {
			return e, err
		}
	}
	if err = event(ctx, tx, p, e, "environment."+action.Kind, map[string]any{"phase": e.Phase}, ""); err != nil {
		return e, err
	}
	if err = audit(ctx, tx, p, "environment."+action.Kind, e.ID, map[string]any{"generation": e.Generation, "version": e.Version, "reason": action.Reason}); err != nil {
		return e, err
	}
	if err = notify(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = revision(ctx, tx, p); err != nil {
		return e, err
	}
	return e, nil
}
func (s *Store) DeployEnvironment(ctx context.Context, p domain.Principal, id string, expectedVersion int64, input domain.DeployInput) (domain.Environment, error) {
	if err := requireHuman(p); err != nil {
		return domain.Environment{}, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := getEnvironment(ctx, tx, id, true)
	if err != nil {
		return e, err
	}
	if e.Version != expectedVersion {
		return e, domain.ErrConflict
	}
	old := e.Version
	e.Generation++
	e.Commit = input.Commit
	e.Spec = input.Spec
	e.BuildState = "ready"
	e.DesiredState = "Running"
	e.Phase = "Pending"
	e.Status = raw(map[string]any{})
	if !input.ExpiresAt.IsZero() {
		e.ExpiresAt = input.ExpiresAt
	}
	e.Spec = mutateSpec(e)
	e, err = updateEnvironment(ctx, tx, e, old)
	if err != nil {
		return e, err
	}
	if err = deployment(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = event(ctx, tx, p, e, "environment.deploy", map[string]any{"commit": e.Commit}, ""); err != nil {
		return e, err
	}
	if err = audit(ctx, tx, p, "environment.deploy", e.ID, map[string]any{"generation": e.Generation}); err != nil {
		return e, err
	}
	if err = notify(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = revision(ctx, tx, p); err != nil {
		return e, err
	}
	if err = completeDelivery(ctx, tx, p, input.DeliveryID, input.LeaseToken); err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func validateStatus(u domain.StatusUpdate) error {
	if u.EventID == "" || len(u.EventID) > 200 {
		return errors.New("status eventId is required and bounded")
	}
	if len(u.Status) > 32768 || !json.Valid(u.Status) {
		return errors.New("status must be a bounded JSON object")
	}
	var status map[string]any
	if json.Unmarshal(u.Status, &status) != nil || status == nil {
		return errors.New("status must be an object")
	}
	if len(u.Diagnoses) > 5 || len(u.SmokeRuns) > 32 {
		return errors.New("status summary exceeds limits")
	}
	for _, d := range u.Diagnoses {
		if len(d.Code) > 64 || len(d.Summary) > 1024 || len(d.Suggestion) > 1024 || len(d.Subject) > 253 || len(d.Stage) > 64 || len(d.Evidence) > 10 {
			return errors.New("diagnosis exceeds limits")
		}
		for _, line := range d.Evidence {
			if len(line) > 256 {
				return errors.New("diagnosis evidence exceeds limits")
			}
		}
	}
	for _, run := range u.SmokeRuns {
		if len(run.Name) > 200 || len(run.Summary) > 1024 || run.DurationMS < 0 {
			return errors.New("smoke run exceeds limits")
		}
	}
	return nil
}
func (s *Store) ReportStatus(ctx context.Context, p domain.Principal, id string, u domain.StatusUpdate) (domain.Environment, error) {
	if p.Role != "agent" || p.ClusterID == "" {
		return domain.Environment{}, domain.ErrForbidden
	}
	if err := validateStatus(u); err != nil {
		return domain.Environment{}, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := getEnvironment(ctx, tx, id, true)
	if err != nil {
		return e, err
	}
	if err = checkEnvironment(p, e); err != nil {
		return e, err
	}
	var priorKind string
	var identical bool
	err = tx.QueryRow(ctx, `SELECT kind,(generation=$3 AND payload=$4::jsonb) FROM heimdall.events WHERE environment_id=$1 AND source_event_id=$2`, id, u.EventID, u.Generation, raw(map[string]any{"phase": u.Phase, "step": u.Step, "status": u.Status})).Scan(&priorKind, &identical)
	if err == nil {
		if priorKind == "stale" {
			return e, domain.ErrStaleGeneration
		}
		if !identical {
			return e, domain.ErrIdempotencyConflict
		}
		return e, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	if u.Generation != e.Generation {
		stale := e
		stale.Generation = u.Generation
		if err = event(ctx, tx, p, stale, "stale", map[string]any{"reportedGeneration": u.Generation, "currentGeneration": e.Generation}, u.EventID); err != nil {
			return e, err
		}
		if err = audit(ctx, tx, p, "status.stale", id, map[string]any{"reportedGeneration": u.Generation, "currentGeneration": e.Generation}); err != nil {
			return e, err
		}
		if err = tx.Commit(ctx); err != nil {
			return e, err
		}
		return e, domain.ErrStaleGeneration
	}
	if u.Version != e.Version {
		return e, domain.ErrConflict
	}
	if e.BuildState != "ready" {
		return e, domain.ErrConflict
	}
	if e.DesiredState == "Destroyed" && u.Phase != "Destroying" && u.Phase != "Destroyed" && u.Phase != "Failed" {
		return e, domain.ErrInvalidTransition
	}
	if e.DesiredState == "Running" && (u.Phase == "Destroying" || u.Phase == "Destroyed") {
		return e, domain.ErrInvalidTransition
	}
	if err = domain.Transition(e.Phase, u.Phase); err != nil {
		// The outbound reporter can reconnect after all intermediate phases have
		// finished. Accept the authoritative successful projection without
		// fabricating stage events that were never reported.
		if u.Phase != "Ready" || e.DesiredState != "Running" || !successfulProjection(u.Status, u.Generation, e.ResetNonce) {
			return e, err
		}
	}
	old := e.Version
	priorStatus := append(json.RawMessage(nil), e.Status...)
	e.Phase = u.Phase
	e.Status = u.Status
	e, err = updateEnvironment(ctx, tx, e, old)
	if err != nil {
		return e, err
	}
	if err = event(ctx, tx, p, e, "status", map[string]any{"phase": u.Phase, "step": u.Step, "status": u.Status}, u.EventID); err != nil {
		return e, err
	}
	if err = audit(ctx, tx, p, "status.report", id, map[string]any{"generation": u.Generation, "phase": u.Phase, "eventId": u.EventID}); err != nil {
		return e, err
	}
	if err = stageEvents(ctx, tx, p, e, priorStatus, u.Status, u.EventID); err != nil {
		return e, err
	}
	for _, d := range u.Diagnoses {
		// Preparation failures can have no workload evidence. Persist their
		// empty collection as an array, as required by the database contract.
		evidence := d.Evidence
		if evidence == nil {
			evidence = []string{}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO heimdall.diagnoses(tenant_id,environment_id,generation,code,summary,suggestion,evidence,subject,stage) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.TenantID, id, e.Generation, d.Code, d.Summary, d.Suggestion, raw(evidence), d.Subject, d.Stage); err != nil {
			return e, err
		}
	}
	for _, run := range u.SmokeRuns {
		if _, err = tx.Exec(ctx, `INSERT INTO heimdall.smoke_runs(tenant_id,environment_id,generation,name,passed,duration_ms,summary) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.TenantID, id, e.Generation, run.Name, run.Passed, run.DurationMS, run.Summary); err != nil {
			return e, err
		}
	}
	if err = notify(ctx, tx, p, e); err != nil {
		return e, err
	}
	if e.Phase == "Destroyed" {
		if err = revision(ctx, tx, p); err != nil {
			return e, err
		}
	}
	return e, tx.Commit(ctx)
}

// A reconnect may deliver several completed steps in one authoritative status
// projection. Record each observed step transition with its real timings; do
// not invent intermediate phases or start times.
func stageEvents(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.Environment, previous, current json.RawMessage, eventID string) error {
	type step struct {
		Name            string     `json:"name"`
		State           string     `json:"state"`
		StartedAt       *time.Time `json:"startedAt,omitempty"`
		DurationSeconds int64      `json:"durationSeconds,omitempty"`
		Code            string     `json:"code,omitempty"`
	}
	type projection struct {
		Steps     []step `json:"steps"`
		Operation *struct {
			Type       string    `json:"type"`
			Generation int64     `json:"generation"`
			ResetNonce int64     `json:"resetNonce"`
			StartedAt  time.Time `json:"startedAt"`
		} `json:"operation"`
	}
	var old, next projection
	if json.Unmarshal(current, &next) != nil {
		return nil
	}
	_ = json.Unmarshal(previous, &old)
	sameOperation := old.Operation != nil && next.Operation != nil && *old.Operation == *next.Operation
	states := map[string]string{}
	if sameOperation {
		for _, item := range old.Steps {
			states[item.Name] = item.State
		}
	}
	if len(next.Steps) > 64 {
		return errors.New("too many status steps")
	}
	for _, item := range next.Steps {
		if item.Name == "" || len(item.Name) > 200 || len(item.State) > 64 || len(item.Code) > 64 || item.DurationSeconds < 0 {
			return errors.New("invalid bounded stage status")
		}
		if states[item.Name] == item.State {
			continue
		}
		hash := sha256.Sum256(raw(struct {
			EventID string
			Step    step
		}{eventID, item}))
		if err := event(ctx, tx, p, e, "stage", map[string]any{"phase": e.Phase, "step": item.Name, "state": item.State, "startedAt": item.StartedAt, "durationSeconds": item.DurationSeconds, "code": item.Code}, fmt.Sprintf("step:%x", hash)); err != nil {
			return err
		}
	}
	return nil
}
func successfulProjection(data json.RawMessage, generation, nonce int64) bool {
	var status struct {
		DeployedGeneration  int64 `json:"deployedGeneration"`
		CompletedResetNonce int64 `json:"completedResetNonce"`
		Operation           *struct {
			Generation int64  `json:"generation"`
			Result     string `json:"result"`
		} `json:"operation"`
	}
	if json.Unmarshal(data, &status) != nil || status.Operation == nil {
		return false
	}
	return status.Operation.Result == "Succeeded" && status.Operation.Generation == generation && (status.DeployedGeneration == generation || nonce > 0 && status.CompletedResetNonce == nonce)
}
func (s *Store) RecordUsage(ctx context.Context, p domain.Principal, u domain.UsageSample) error {
	if p.Role != "agent" {
		return domain.ErrForbidden
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := getEnvironment(ctx, tx, u.EnvironmentID, false)
	if err != nil {
		return err
	}
	if err = checkEnvironment(p, e); err != nil {
		return err
	}
	if u.At.IsZero() {
		u.At = time.Now().UTC()
	}
	if _, err = tx.Exec(ctx, `INSERT INTO heimdall.usage_samples(tenant_id,environment_id,cpu_milli,memory_mi,storage_mi,sampled_at) VALUES($1,$2,$3,$4,$5,$6)`, p.TenantID, u.EnvironmentID, u.CPUMilli, u.MemoryMi, u.StorageMi, u.At); err != nil {
		return err
	}
	if err = audit(ctx, tx, p, "usage.record", e.ID, map[string]any{"sampledAt": u.At}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
