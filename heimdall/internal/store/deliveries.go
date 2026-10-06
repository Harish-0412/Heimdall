package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (s *Store) IngestWebhook(ctx context.Context, p domain.Principal, d domain.WebhookDelivery) (bool, error) {
	if err := requireAdmin(p); err != nil {
		return false, err
	}
	if d.TenantID != "" && d.TenantID != p.TenantID {
		return false, domain.ErrForbidden
	}
	if d.DeliveryID == "" || len(d.DeliveryID) > 200 || d.Event == "" || len(d.Event) > 64 || len(d.Payload) > 1048576 || !json.Valid(d.Payload) {
		return false, errors.New("invalid bounded webhook delivery")
	}
	hash := sha256.Sum256(d.Payload)
	actual := hex.EncodeToString(hash[:])
	if d.PayloadHash != "" && d.PayloadHash != actual {
		return false, errors.New("webhook payload hash mismatch")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var repo any
	if d.RepositoryID != "" {
		repo = d.RepositoryID
	}
	tag, err := tx.Exec(ctx, `INSERT INTO heimdall.webhook_deliveries(delivery_id,tenant_id,event,repository_id,pull_request,payload,payload_hash) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(delivery_id) DO NOTHING`, d.DeliveryID, p.TenantID, d.Event, repo, d.PullRequest, d.Payload, actual)
	if err != nil {
		return false, translate(err)
	}
	if tag.RowsAffected() == 0 {
		var saved string
		if err = tx.QueryRow(ctx, `SELECT payload_hash FROM heimdall.webhook_deliveries WHERE delivery_id=$1`, d.DeliveryID).Scan(&saved); err != nil {
			return false, translate(err)
		}
		if saved != actual {
			return false, domain.ErrIdempotencyConflict
		}
		return false, nil
	}
	if err = audit(ctx, tx, p, "webhook.received", d.DeliveryID, map[string]any{"event": d.Event, "repositoryId": d.RepositoryID, "pullRequest": d.PullRequest}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) ClaimWebhook(ctx context.Context, p domain.Principal, id string) (domain.WebhookDelivery, error) {
	return s.ClaimWebhookWithLease(ctx, p, id, 2*time.Minute)
}
func (s *Store) ClaimWebhookWithLease(ctx context.Context, p domain.Principal, id string, lease time.Duration) (domain.WebhookDelivery, error) {
	var d domain.WebhookDelivery
	if err := requireAdmin(p); err != nil {
		return d, err
	}
	if !validLease(lease) {
		return d, errors.New("invalid delivery lease")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return d, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	token := uuid.NewString()
	_, err = tx.Exec(ctx, `UPDATE heimdall.webhook_deliveries SET state='dead',lease_token=NULL,lease_until=NULL,last_error='delivery attempts exhausted' WHERE delivery_id=$1 AND attempts>=12 AND (state='pending' OR state='processing' AND lease_until<now())`, id)
	if err != nil {
		return d, err
	}
	// Ordering belongs to SQS FIFO. A poison message moved to its DLQ must
	// never leave a pending database row that blocks this PR forever.
	err = tx.QueryRow(ctx, `UPDATE heimdall.webhook_deliveries d SET state='processing',lease_token=$2,lease_until=now()+($3::bigint*interval '1 millisecond'),attempts=attempts+1,worker=$4 WHERE d.delivery_id=$1 AND d.attempts<12 AND d.available_at<=now() AND (d.state='pending' OR (d.state='processing' AND d.lease_until<now())) RETURNING delivery_id,tenant_id::text,event,COALESCE(repository_id::text,''),pull_request,payload,payload_hash,lease_token,attempts,received_at`, id, token, lease.Milliseconds(), p.ActorID).Scan(&d.DeliveryID, &d.TenantID, &d.Event, &d.RepositoryID, &d.PullRequest, &d.Payload, &d.PayloadHash, &d.LeaseToken, &d.Attempts, &d.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var state string
		lookupErr := tx.QueryRow(ctx, `SELECT state FROM heimdall.webhook_deliveries WHERE delivery_id=$1`, id).Scan(&state)
		if lookupErr == nil && (state == "pending" || state == "processing") {
			return d, domain.ErrConflict
		}
		if lookupErr == nil && state == "dead" {
			if err = tx.Commit(ctx); err != nil {
				return d, err
			}
			return d, domain.ErrNotFound
		}
		return d, domain.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}
func completeDelivery(ctx context.Context, tx pgx.Tx, p domain.Principal, id, token string) error {
	if id == "" {
		return nil
	}
	if token == "" {
		return domain.ErrLeaseLost
	}
	tag, err := tx.Exec(ctx, `UPDATE heimdall.webhook_deliveries SET state='complete',completed_at=now(),lease_token=NULL,lease_until=NULL WHERE delivery_id=$1 AND state='processing' AND lease_token=$2 AND lease_until>now()`, id, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return nil
}
func (s *Store) CompleteWebhook(ctx context.Context, p domain.Principal, id, token string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = completeDelivery(ctx, tx, p, id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RetryWebhook(ctx context.Context, p domain.Principal, id, token, reason string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE heimdall.webhook_deliveries SET state=CASE WHEN attempts>=12 THEN 'dead' ELSE 'pending' END,available_at=now()+(LEAST(300,power(2,attempts))::int*interval '1 second'),last_error=$3,lease_token=NULL,lease_until=NULL WHERE delivery_id=$1 AND state='processing' AND lease_token=$2 AND lease_until>now()`, id, token, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return tx.Commit(ctx)
}
func (s *Store) ClaimOutbox(ctx context.Context, p domain.Principal, limit int) ([]domain.Outbox, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	token := uuid.NewString()
	if _, err = tx.Exec(ctx, `UPDATE heimdall.outbox SET state='dead',lease_token=NULL,lease_until=NULL,last_error='delivery attempts exhausted' WHERE attempts>=12 AND (state='pending' OR state='processing' AND lease_until<now())`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH work AS(SELECT d.id FROM heimdall.outbox d WHERE d.attempts<12 AND d.available_at<=now() AND (d.state='pending' OR (d.state='processing' AND d.lease_until<now())) AND NOT EXISTS(SELECT FROM heimdall.outbox prior WHERE prior.id<d.id AND prior.environment_id IS NOT DISTINCT FROM d.environment_id AND prior.state IN ('pending','processing')) ORDER BY d.id LIMIT $1 FOR UPDATE SKIP LOCKED) UPDATE heimdall.outbox d SET state='processing',lease_token=$2,lease_until=now()+interval '2 minutes',worker=$3,attempts=attempts+1 FROM work WHERE d.id=work.id RETURNING d.id,d.tenant_id::text,COALESCE(d.environment_id,''),d.generation,d.kind,d.payload,d.lease_token,d.attempts`, pageLimit(limit), token, p.ActorID)
	if err != nil {
		return nil, err
	}
	list := []domain.Outbox{}
	for rows.Next() {
		var d domain.Outbox
		if err = rows.Scan(&d.ID, &d.TenantID, &d.EnvironmentID, &d.Generation, &d.Kind, &d.Payload, &d.LeaseToken, &d.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return list, tx.Commit(ctx)
}
func (s *Store) RetryOutbox(ctx context.Context, p domain.Principal, id int64, token string, delay time.Duration, reason string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if delay < 0 || delay > time.Hour {
		return errors.New("invalid retry delay")
	}
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE heimdall.outbox SET state=CASE WHEN attempts>=12 THEN 'dead' ELSE 'pending' END,available_at=now()+($3::bigint*interval '1 millisecond'),last_error=$4,lease_token=NULL,lease_until=NULL WHERE id=$1 AND state='processing' AND lease_token=$2 AND lease_until>now()`, id, token, delay.Milliseconds(), reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return tx.Commit(ctx)
}
func getGitHubDelivery(ctx context.Context, tx pgx.Tx, id string) (domain.GitHubDelivery, error) {
	d := domain.GitHubDelivery{EnvironmentID: id}
	err := tx.QueryRow(ctx, `SELECT generation,comment_id,check_id FROM heimdall.github_delivery_state WHERE environment_id=$1`, id).Scan(&d.Generation, &d.CommentID, &d.CheckID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	return d, err
}
func (s *Store) GetGitHubDelivery(ctx context.Context, p domain.Principal, id string) (domain.GitHubDelivery, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.GitHubDelivery{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = getEnvironment(ctx, tx, id, false); err != nil {
		return domain.GitHubDelivery{}, err
	}
	return getGitHubDelivery(ctx, tx, id)
}
func putGitHubDelivery(ctx context.Context, tx pgx.Tx, p domain.Principal, d domain.GitHubDelivery) error {
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.github_delivery_state(tenant_id,environment_id,generation,comment_id,check_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,environment_id) DO UPDATE SET generation=EXCLUDED.generation,comment_id=EXCLUDED.comment_id,check_id=EXCLUDED.check_id,updated_at=now() WHERE heimdall.github_delivery_state.generation<=EXCLUDED.generation`, p.TenantID, d.EnvironmentID, d.Generation, d.CommentID, d.CheckID)
	return err
}

// A bounded callback serializes an external GitHub write with every intent and
// status mutation. A crash after the write is recovered using GitHub's stable
// comment marker/check external ID before this transaction records its IDs.
func (s *Store) WithGitHubDelivery(ctx context.Context, p domain.Principal, id string, fn func(domain.Environment, domain.GitHubDelivery) (domain.GitHubDelivery, error)) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := getEnvironment(ctx, tx, id, true)
	if err != nil {
		return err
	}
	d, err := getGitHubDelivery(ctx, tx, id)
	if err != nil {
		return err
	}
	d, err = fn(e, d)
	if err != nil {
		return err
	}
	if d.EnvironmentID != id || d.Generation != e.Generation {
		return domain.ErrStaleGeneration
	}
	if err = putGitHubDelivery(ctx, tx, p, d); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) WithGitHubNotice(ctx context.Context, p domain.Principal, repoID string, number int64, fn func(domain.Repository, domain.PullRequest, *domain.Environment) error) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo, err := scanRepository(tx.QueryRow(ctx, `SELECT `+repositoryColumns+` FROM heimdall.repositories WHERE id=$1 FOR UPDATE`, repoID))
	if err != nil {
		return err
	}
	pr, err := getPullRequest(ctx, tx, repoID, number, true)
	if err != nil {
		return err
	}
	e, err := scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE repository_id=$1 AND pull_request=$2 FOR UPDATE`, repoID, number))
	var existing *domain.Environment
	if err == nil {
		existing = &e
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err = fn(repo, pr, existing); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) FinishOutbox(ctx context.Context, p domain.Principal, id int64, token string, generation int64, d domain.GitHubDelivery) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var envID *string
	var savedGeneration int64
	if err = tx.QueryRow(ctx, `SELECT environment_id,generation FROM heimdall.outbox WHERE id=$1 AND state='processing' AND lease_token=$2 AND lease_until>now() FOR UPDATE`, id, token).Scan(&envID, &savedGeneration); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLeaseLost
		}
		return err
	}
	if savedGeneration != generation {
		return domain.ErrStaleGeneration
	}
	if envID != nil && d.EnvironmentID != "" {
		e, err := getEnvironment(ctx, tx, *envID, true)
		if err != nil {
			return err
		}
		if e.Generation == generation {
			if d.EnvironmentID != e.ID || d.Generation != generation {
				return domain.ErrStaleGeneration
			}
			if err = putGitHubDelivery(ctx, tx, p, d); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE heimdall.outbox SET state='complete',completed_at=now(),lease_token=NULL,lease_until=NULL WHERE id=$1`, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RecordCommand(ctx context.Context, p domain.Principal, id, token, repoID string, number, commentID int64, command, outcome, response string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if len(response) > 16384 || len(command) > 200 || len(outcome) > 64 {
		return errors.New("command response exceeds limits")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	metadata := map[string]any{"repositoryId": repoID, "pullRequest": number, "commentId": commentID, "command": command, "outcome": outcome}
	if err = audit(ctx, tx, p, "github.command", id, metadata); err != nil {
		return err
	}
	if response != "" {
		_, err = tx.Exec(ctx, `INSERT INTO heimdall.outbox(tenant_id,kind,payload) VALUES($1,'github.command',$2)`, p.TenantID, raw(map[string]any{"repositoryID": repoID, "pullRequest": number, "sourceCommentID": commentID, "deliveryID": id, "body": response}))
		if err != nil {
			return err
		}
	}
	if err = completeDelivery(ctx, tx, p, id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Audit(ctx context.Context, p domain.Principal, action, resource string, metadata json.RawMessage) error {
	return s.AppendAudit(ctx, p, action, resource, metadata)
}
func equalHash(a, b []byte) bool { return bytes.Equal(a, b) }
