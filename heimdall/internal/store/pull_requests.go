package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"github.com/jackc/pgx/v5"
)

const prColumns = `repository_id::text,number,head_sha,base_sha,state,is_fork,needs_approval,approved_sha,config_digest,baseline_digest,updated_at,observed_at,version,trace_parent`

func getPullRequest(ctx context.Context, tx pgx.Tx, repo string, number int64, lock bool) (domain.PullRequest, error) {
	var pr domain.PullRequest
	q := `SELECT ` + prColumns + ` FROM heimdall.pull_requests WHERE repository_id=$1 AND number=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, q, repo, number).Scan(&pr.RepositoryID, &pr.Number, &pr.HeadSHA, &pr.BaseSHA, &pr.State, &pr.Fork, &pr.NeedsApproval, &pr.ApprovedSHA, &pr.ConfigDigest, &pr.BaselineDigest, &pr.UpdatedAt, &pr.ObservedAt, &pr.Version, &pr.TraceParent)
	return pr, translate(err)
}
func (s *Store) GetPullRequest(ctx context.Context, p domain.Principal, repo string, number int64) (domain.PullRequest, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.PullRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return getPullRequest(ctx, tx, repo, number, false)
}
func (s *Store) EnvironmentByPullRequest(ctx context.Context, p domain.Principal, repo string, number int64) (domain.Environment, error) {
	return s.GetEnvironmentByPR(ctx, p, repo, number)
}
func (s *Store) ObservePullRequest(ctx context.Context, p domain.Principal, o domain.PullRequestObservation) (domain.Environment, error) {
	var e domain.Environment
	if err := requireAdmin(p); err != nil {
		return e, err
	}
	if o.Number < 1 || o.HeadSHA == "" || (o.State != "open" && o.State != "closed" && o.State != "refused") {
		return e, errors.New("invalid pull request observation")
	}
	if err := tracecontext.Validate(o.TraceParent); err != nil {
		return e, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return e, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock repository first: observations, approval and builds share this order,
	// including the first observation before a PR row exists.
	repo, err := scanRepository(tx.QueryRow(ctx, `SELECT `+repositoryColumns+` FROM heimdall.repositories WHERE id=$1 FOR UPDATE`, o.RepositoryID))
	if err != nil {
		return e, err
	}
	oldPR, err := getPullRequest(ctx, tx, o.RepositoryID, o.Number, true)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return e, err
	}
	first := errors.Is(err, domain.ErrNotFound)
	if first && o.ExpectedVersion != 0 || !first && o.ExpectedVersion != oldPR.Version {
		return e, domain.ErrConflict
	}
	if !first && !o.UpdatedAt.IsZero() && o.UpdatedAt.Before(oldPR.ObservedAt) {
		if err = audit(ctx, tx, p, "github.pr.stale", fmt.Sprintf("%s#%d", repo.ID, o.Number), map[string]any{"head": o.HeadSHA, "observedAt": o.UpdatedAt}); err != nil {
			return e, err
		}
		if err = completeDelivery(ctx, tx, p, o.DeliveryID, o.LeaseToken); err != nil {
			return e, err
		}
		e, _ = scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE repository_id=$1 AND pull_request=$2`, repo.ID, o.Number))
		return e, tx.Commit(ctx)
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = time.Now().UTC()
	}
	approved := ""
	if !first && oldPR.HeadSHA == o.HeadSHA {
		approved = oldPR.ApprovedSHA
	}
	state := o.State
	if o.IsFork || !repo.Enabled {
		state = "refused"
	}
	if !first && oldPR.HeadSHA == o.HeadSHA && oldPR.State == state && oldPR.ConfigDigest == o.ConfigDigest && oldPR.BaselineDigest == o.BaselineDigest && oldPR.BaseSHA == o.BaseSHA {
		o.TraceParent = oldPR.TraceParent
	}
	_, err = tx.Exec(ctx, `INSERT INTO heimdall.pull_requests(tenant_id,repository_id,number,head_sha,base_sha,state,is_fork,needs_approval,approved_sha,config_digest,baseline_digest,observed_at,trace_parent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(tenant_id,repository_id,number) DO UPDATE SET head_sha=EXCLUDED.head_sha,base_sha=EXCLUDED.base_sha,state=EXCLUDED.state,is_fork=EXCLUDED.is_fork,needs_approval=EXCLUDED.needs_approval,approved_sha=EXCLUDED.approved_sha,config_digest=EXCLUDED.config_digest,baseline_digest=EXCLUDED.baseline_digest,observed_at=EXCLUDED.observed_at,trace_parent=EXCLUDED.trace_parent,version=heimdall.pull_requests.version+1,updated_at=now()`, p.TenantID, o.RepositoryID, o.Number, o.HeadSHA, o.BaseSHA, state, o.IsFork, o.NeedsApproval, approved, o.ConfigDigest, o.BaselineDigest, o.UpdatedAt, o.TraceParent)
	if err != nil {
		return e, err
	}
	if o.Message != "" {
		if len(o.Message) > 16384 {
			return e, errors.New("notice exceeds limits")
		}
		_, err = tx.Exec(ctx, `INSERT INTO heimdall.outbox(tenant_id,kind,payload) VALUES($1,'github.notice',$2)`, p.TenantID, raw(map[string]any{"repositoryID": repo.ID, "pullRequest": o.Number, "deliveryID": o.DeliveryID, "body": o.Message}))
		if err != nil {
			return e, err
		}
	}
	e, err = scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE repository_id=$1 AND pull_request=$2 FOR UPDATE`, o.RepositoryID, o.Number))
	switch {
	case errors.Is(err, domain.ErrNotFound):
		if state != "open" {
			if err = audit(ctx, tx, p, "github.pr.observed", fmt.Sprintf("%s#%d", repo.ID, o.Number), map[string]any{"state": state, "fork": o.IsFork, "head": o.HeadSHA}); err != nil {
				return e, err
			}
			if err = completeDelivery(ctx, tx, p, o.DeliveryID, o.LeaseToken); err != nil {
				return e, err
			}
			return e, tx.Commit(ctx)
		}
		if err = capacity(ctx, tx, p); err != nil {
			return e, err
		}
		policy, err := getPolicy(ctx, tx, p)
		if err != nil {
			return e, err
		}
		e, err = insertEnvironment(ctx, tx, p, domain.CreateEnvironment{RepositoryID: repo.ID, ClusterID: repo.ClusterID, PullRequest: o.Number, Owner: o.Owner, Commit: o.HeadSHA, ExpiresAt: time.Now().UTC().Add(policy.MaxTTL)}, "pending")
		if err != nil {
			return e, err
		}
	case err != nil:
		return e, err
	case state != "open":
		if e.DesiredState != "Destroyed" {
			if string(e.Spec) == "{}" {
				old := e.Version
				e.Generation++
				e.DesiredState = "Destroyed"
				e.Phase = "Destroyed"
				e.BuildState = "refused"
				e, err = updateEnvironment(ctx, tx, e, old)
			} else {
				e, err = act(ctx, tx, p, e, domain.Action{Kind: "delete", Reason: "pull request " + state})
			}
			if err != nil {
				return e, err
			}
		}
	case first || oldPR.HeadSHA != o.HeadSHA || oldPR.State != "open" || oldPR.ConfigDigest != o.ConfigDigest || oldPR.BaselineDigest != o.BaselineDigest || oldPR.BaseSHA != o.BaseSHA:
		if e.DesiredState == "Destroyed" {
			if err = capacity(ctx, tx, p); err != nil {
				return e, err
			}
		}
		old := e.Version
		e.Generation++
		e.Commit = o.HeadSHA
		e.DesiredState = "Running"
		e.Phase = "Pending"
		e.BuildState = "pending"
		e.Status = raw(map[string]any{})
		// Keep the last approved runtime projection until the new build completes.
		// Its spec generation intentionally remains the prior committed generation.
		e, err = updateEnvironment(ctx, tx, e, old)
		if err != nil {
			return e, err
		}
	}
	if e.ID != "" {
		if err = event(ctx, tx, p, e, "github.pr.observed", map[string]any{"state": state, "head": o.HeadSHA, "buildState": e.BuildState, "approvalRequired": o.NeedsApproval && approved != o.HeadSHA}, ""); err != nil {
			return e, err
		}
		if err = notify(ctx, tx, p, e); err != nil {
			return e, err
		}
		if err = revision(ctx, tx, p); err != nil {
			return e, err
		}
	}
	if err = audit(ctx, tx, p, "github.pr.observed", fmt.Sprintf("%s#%d", repo.ID, o.Number), map[string]any{"state": state, "fork": o.IsFork, "head": o.HeadSHA}); err != nil {
		return e, err
	}
	if err = completeDelivery(ctx, tx, p, o.DeliveryID, o.LeaseToken); err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func (s *Store) ApprovePullRequest(ctx context.Context, p domain.Principal, repo string, number int64, head, id, token string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM heimdall.repositories WHERE id=$1 FOR UPDATE`, repo).Scan(&locked); err != nil {
		return translate(err)
	}
	pr, err := getPullRequest(ctx, tx, repo, number, true)
	if err != nil {
		return err
	}
	if pr.HeadSHA != head {
		return domain.ErrStaleGeneration
	}
	if pr.State != "open" || pr.Fork || !pr.NeedsApproval {
		return domain.ErrInvalidTransition
	}
	_, err = tx.Exec(ctx, `UPDATE heimdall.pull_requests SET approved_sha=$3,version=version+1,updated_at=now() WHERE repository_id=$1 AND number=$2`, repo, number, head)
	if err != nil {
		return err
	}
	if err = audit(ctx, tx, p, "github.pr.approve", fmt.Sprintf("%s#%d", repo, number), map[string]any{"head": head}); err != nil {
		return err
	}
	if err = completeDelivery(ctx, tx, p, id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) CommitBuild(ctx context.Context, p domain.Principal, b domain.BuildInput) (domain.Environment, error) {
	var e domain.Environment
	if p.Role != "system" && p.Role != "admin" && p.Role != "ci" {
		return e, domain.ErrForbidden
	}
	if b.Number < 1 || b.HeadSHA == "" || b.RunID == "" || b.RunAttempt < 1 || b.Bundle == "" || len(b.Spec) == 0 {
		return e, errors.New("invalid build input")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return e, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo, err := scanRepository(tx.QueryRow(ctx, `SELECT `+repositoryColumns+` FROM heimdall.repositories WHERE id=$1 FOR UPDATE`, b.RepositoryID))
	if err != nil {
		return e, err
	}
	if b.PolicySHA256 != "" {
		var locked string
		if err = tx.QueryRow(ctx, `SELECT tenant_id::text FROM heimdall.tenant_policy WHERE tenant_id=$1 FOR UPDATE`, p.TenantID).Scan(&locked); err != nil {
			return e, translate(err)
		}
		policy, err := getPolicy(ctx, tx, p)
		if err != nil {
			return e, err
		}
		digest := sha256.Sum256(raw(policy))
		if b.PolicySHA256 != hex.EncodeToString(digest[:]) {
			return e, domain.ErrConflict
		}
	}
	pr, err := getPullRequest(ctx, tx, b.RepositoryID, b.Number, true)
	if err != nil {
		return e, err
	}
	if !repo.Enabled || pr.State != "open" || pr.Fork || pr.HeadSHA != b.HeadSHA || pr.ConfigDigest != b.ConfigDigest || pr.BaselineDigest != b.BaselineDigest {
		return e, domain.ErrStaleGeneration
	}
	if pr.NeedsApproval && pr.ApprovedSHA != b.HeadSHA {
		return e, domain.ErrForbidden
	}
	if repo.TrustedWorkflowRef == "" || repo.TrustedWorkflowRef != b.WorkflowRef {
		return e, domain.ErrForbidden
	}
	e, err = scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE repository_id=$1 AND pull_request=$2 FOR UPDATE`, b.RepositoryID, b.Number))
	if err != nil {
		return e, err
	}
	// CI retries contain the same immutable artifacts, while the normalized
	// projection's expiry is calculated by the server and can differ on replay.
	hash := sha256.Sum256(raw(struct {
		RepositoryID                                                      string
		Number                                                            int64
		HeadSHA, Bundle, ConfigDigest, BaselineDigest, RunID, WorkflowRef string
		RunAttempt                                                        int64
		Images                                                            map[string]string
	}{b.RepositoryID, b.Number, b.HeadSHA, b.Bundle, b.ConfigDigest, b.BaselineDigest, b.RunID, b.WorkflowRef, b.RunAttempt, b.Images}))
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT request_hash FROM heimdall.build_receipts WHERE repository_id=$1 AND run_id=$2 AND run_attempt=$3`, b.RepositoryID, b.RunID, b.RunAttempt).Scan(&saved)
	if err == nil {
		if !equalHash(hash[:], saved) {
			return e, domain.ErrIdempotencyConflict
		}
		return e, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	if e.Commit != b.HeadSHA || e.BuildState != "pending" || e.DesiredState != "Running" {
		return e, domain.ErrStaleGeneration
	}
	old := e.Version
	e.Spec = b.Spec
	e.BuildState = "ready"
	e.Phase = "Pending"
	e.Status = raw(map[string]any{})
	if !b.ExpiresAt.IsZero() {
		e.ExpiresAt = b.ExpiresAt.UTC()
	}
	e.Spec = mutateSpec(e)
	e, err = updateEnvironment(ctx, tx, e, old)
	if err != nil {
		return e, err
	}
	if err = deployment(ctx, tx, p, e); err != nil {
		return e, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO heimdall.build_receipts(tenant_id,repository_id,pull_request,run_id,run_attempt,request_hash,environment_id,generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, p.TenantID, b.RepositoryID, b.Number, b.RunID, b.RunAttempt, hash[:], e.ID, e.Generation)
	if err != nil {
		return e, err
	}
	if err = event(ctx, tx, p, e, "build.ready", map[string]any{"runId": b.RunID, "bundle": b.Bundle}, ""); err != nil {
		return e, err
	}
	if err = audit(ctx, tx, p, "build.commit", e.ID, map[string]any{"generation": e.Generation, "runId": b.RunID, "runAttempt": b.RunAttempt, "bundle": b.Bundle}); err != nil {
		return e, err
	}
	if err = notify(ctx, tx, p, e); err != nil {
		return e, err
	}
	if err = revision(ctx, tx, p); err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}
func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && len(value) == 64
}
func (s *Store) PutDataAttestation(ctx context.Context, p domain.Principal, a domain.DataAttestation) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if a.PullRequest < 1 || !validDigest(a.ConfigSHA256) || !validDigest(a.SeedSHA256) || !a.Sanitised || a.Reason == "" || len(a.Reason) > 1024 || !a.ExpiresAt.After(time.Now()) || a.ExpiresAt.After(time.Now().Add(30*24*time.Hour)) {
		return errors.New("invalid bounded sanitized data attestation")
	}
	a.ApprovedBy = p.ActorID
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A new attestation supersedes any earlier seed for this exact config.
	_, err = tx.Exec(ctx, `UPDATE heimdall.data_attestations SET expires_at=LEAST(expires_at,now()) WHERE repository_id=$1 AND pull_request=$2 AND config_sha256=$3 AND seed_sha256<>$4`, a.RepositoryID, a.PullRequest, a.ConfigSHA256, a.SeedSHA256)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO heimdall.data_attestations(tenant_id,repository_id,pull_request,config_sha256,seed_sha256,approved_by,reason,sanitised,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,true,$8) ON CONFLICT(tenant_id,repository_id,pull_request,config_sha256,seed_sha256) DO UPDATE SET approved_by=EXCLUDED.approved_by,reason=EXCLUDED.reason,expires_at=EXCLUDED.expires_at`, p.TenantID, a.RepositoryID, a.PullRequest, a.ConfigSHA256, a.SeedSHA256, a.ApprovedBy, a.Reason, a.ExpiresAt)
	if err != nil {
		return translate(err)
	}
	if err = audit(ctx, tx, p, "data.attest", fmt.Sprintf("%s#%d", a.RepositoryID, a.PullRequest), a); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) GetDataAttestationForConfig(ctx context.Context, p domain.Principal, repo string, number int64, configSHA string) (domain.DataAttestation, error) {
	var a domain.DataAttestation
	tx, err := s.begin(ctx, p)
	if err != nil {
		return a, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `SELECT repository_id::text,pull_request,config_sha256,seed_sha256,approved_by,reason,sanitised,expires_at FROM heimdall.data_attestations WHERE repository_id=$1 AND pull_request=$2 AND config_sha256=$3 AND expires_at>now() ORDER BY created_at DESC LIMIT 1`, repo, number, configSHA).Scan(&a.RepositoryID, &a.PullRequest, &a.ConfigSHA256, &a.SeedSHA256, &a.ApprovedBy, &a.Reason, &a.Sanitised, &a.ExpiresAt)
	return a, translate(err)
}
func (s *Store) GetDataAttestation(ctx context.Context, p domain.Principal, repo string, number int64, configSHA, seedSHA string) (domain.DataAttestation, error) {
	var a domain.DataAttestation
	tx, err := s.begin(ctx, p)
	if err != nil {
		return a, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `SELECT repository_id::text,pull_request,config_sha256,seed_sha256,approved_by,reason,sanitised,expires_at FROM heimdall.data_attestations WHERE repository_id=$1 AND pull_request=$2 AND config_sha256=$3 AND seed_sha256=$4 AND expires_at>now()`, repo, number, configSHA, seedSHA).Scan(&a.RepositoryID, &a.PullRequest, &a.ConfigSHA256, &a.SeedSHA256, &a.ApprovedBy, &a.Reason, &a.Sanitised, &a.ExpiresAt)
	return a, translate(err)
}
