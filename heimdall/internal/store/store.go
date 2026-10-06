// Package store is the transactional PostgreSQL boundary. Every tenant read and
// write uses SET LOCAL from a verified principal, never a request tenant field.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/heimdall-dev/heimdall/db/migrations"
	"github.com/heimdall-dev/heimdall/internal/domain"
)

type Store struct{ pool *pgxpool.Pool }

// Migrate requires an operator/migration credential. New rejects that role for
// application traffic; migrations are deliberately separate from API startup.
func Migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool}
	if err = s.verifyRole(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) verifyRole(ctx context.Context) error {
	var unsafe bool
	err := s.pool.QueryRow(ctx, `SELECT r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR EXISTS(SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='heimdall' AND pg_has_role(current_user,c.relowner,'MEMBER')) FROM pg_roles r WHERE r.rolname=current_user`).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return errors.New("application database role must not own control-plane tables, be superuser, BYPASSRLS, or inherit owner privileges")
	}
	return nil
}
func validatePrincipal(p domain.Principal) error {
	if _, err := uuid.Parse(p.TenantID); err != nil || p.ActorID == "" {
		return domain.ErrUnauthorized
	}
	switch p.Role {
	case "admin", "member", "agent", "ci", "system":
	default:
		return domain.ErrUnauthorized
	}
	return nil
}
func (s *Store) begin(ctx context.Context, p domain.Principal) (pgx.Tx, error) {
	if err := validatePrincipal(p); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, p.TenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func requireAdmin(p domain.Principal) error {
	if p.Role != "admin" && p.Role != "system" {
		return domain.ErrForbidden
	}
	return nil
}
func requireHuman(p domain.Principal) error {
	if p.Role != "admin" && p.Role != "member" && p.Role != "system" {
		return domain.ErrForbidden
	}
	return nil
}
func checkEnvironment(p domain.Principal, e domain.Environment) error {
	if p.Role == "agent" && (p.ClusterID == "" || p.ClusterID != e.ClusterID) {
		return domain.ErrNotFound
	}
	return nil
}
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		switch pgerr.Code {
		case "23505":
			return fmt.Errorf("%w: duplicate resource", domain.ErrConflict)
		case "23503", "42501":
			return domain.ErrForbidden
		}
	}
	return err
}
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func audit(ctx context.Context, tx pgx.Tx, p domain.Principal, action, resource string, metadata any) error {
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.audit_log(tenant_id,actor_id,action,resource,metadata) VALUES($1,$2,$3,$4,$5)`, p.TenantID, p.ActorID, action, resource, raw(metadata))
	return err
}
func revision(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	_, err := tx.Exec(ctx, `UPDATE heimdall.tenant_policy SET desired_revision=desired_revision+1 WHERE tenant_id=$1`, p.TenantID)
	return err
}
func event(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.Environment, kind string, payload any, sourceID string) error {
	var source any
	if sourceID != "" {
		source = sourceID
	}
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.events(tenant_id,environment_id,generation,kind,payload,source_event_id) VALUES($1,$2,$3,$4,$5,$6)`, p.TenantID, e.ID, e.Generation, kind, raw(payload), source)
	return err
}
func notify(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.Environment) error {
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.outbox(tenant_id,environment_id,generation,kind,payload) VALUES($1,$2,$3,'github.preview',$4)`, p.TenantID, e.ID, e.Generation, raw(map[string]any{"environmentId": e.ID, "generation": e.Generation}))
	return err
}
func (s *Store) AppendAudit(ctx context.Context, p domain.Principal, action, resource string, metadata json.RawMessage) error {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = audit(ctx, tx, p, action, resource, metadata); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ProvisionTenant is an explicit operator bootstrap, using the migration DSN.
// No HTTP endpoint may accept this DSN, and application-role New rejects it.
func ProvisionTenant(ctx context.Context, dsn string, t domain.Tenant, policy json.RawMessage, q domain.Quota) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, t.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO heimdall.tenants(id,slug,name) VALUES($1,$2,$3)`, t.ID, t.Slug, t.Name); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO heimdall.tenant_policy(tenant_id,policy) VALUES($1,$2)`, t.ID, policy); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO heimdall.quotas(tenant_id,max_environments,max_cpu_milli,max_memory_mi,max_storage_mi) VALUES($1,$2,$3,$4,$5)`, t.ID, q.MaxEnvironments, q.MaxCPUMilli, q.MaxMemoryMi, q.MaxStorageMi); err != nil {
		return err
	}
	if err = audit(ctx, tx, domain.Principal{TenantID: t.ID, ActorID: "operator", Role: "admin"}, "tenant.provision", t.ID, map[string]any{"slug": t.Slug}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ActiveTenants(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT heimdall.active_tenants()::text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) ResolveInstallation(ctx context.Context, id int64) (string, error) {
	var tenant *string
	err := s.pool.QueryRow(ctx, `SELECT heimdall.resolve_installation($1)::text`, id).Scan(&tenant)
	if err != nil {
		return "", err
	}
	if tenant == nil {
		return "", domain.ErrNotFound
	}
	return *tenant, nil
}

const environmentColumns = `id,tenant_id::text,repository_id::text,cluster_id::text,name,pull_request,owner,commit_sha,version,generation,reset_nonce,desired_state,phase,build_state,spec,status,expires_at,created_at,updated_at`

func scanEnvironment(row pgx.Row) (domain.Environment, error) {
	var e domain.Environment
	err := row.Scan(&e.ID, &e.TenantID, &e.RepositoryID, &e.ClusterID, &e.Name, &e.PullRequest, &e.Owner, &e.Commit, &e.Version, &e.Generation, &e.ResetNonce, &e.DesiredState, &e.Phase, &e.BuildState, &e.Spec, &e.Status, &e.ExpiresAt, &e.CreatedAt, &e.UpdatedAt)
	return e, translate(err)
}
func getEnvironment(ctx context.Context, tx pgx.Tx, id string, lock bool) (domain.Environment, error) {
	query := `SELECT ` + environmentColumns + ` FROM heimdall.environments WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanEnvironment(tx.QueryRow(ctx, query, id))
}
func (s *Store) GetEnvironment(ctx context.Context, p domain.Principal, id string) (domain.Environment, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer tx.Rollback(ctx)
	e, err := getEnvironment(ctx, tx, id, false)
	if err == nil {
		err = checkEnvironment(p, e)
	}
	return e, err
}
func (s *Store) GetEnvironmentByPR(ctx context.Context, p domain.Principal, repoID string, number int64) (domain.Environment, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Environment{}, err
	}
	defer tx.Rollback(ctx)
	e, err := scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE repository_id=$1 AND pull_request=$2`, repoID, number))
	if err == nil {
		err = checkEnvironment(p, e)
	}
	return e, err
}
func pageLimit(n int) int {
	if n < 1 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}
func (s *Store) ListEnvironments(ctx context.Context, p domain.Principal, page domain.Page) ([]domain.Environment, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if p.Role == "agent" {
		page.ClusterID = p.ClusterID
		if page.ClusterID == "" {
			return nil, domain.ErrForbidden
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE id>$1 AND ($2='' OR repository_id::text=$2) AND ($3='' OR cluster_id::text=$3) ORDER BY id LIMIT $4`, page.AfterID, page.RepositoryID, page.ClusterID, pageLimit(page.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []domain.Environment{}
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}
func (s *Store) Timeline(ctx context.Context, p domain.Principal, id string, after int64, limit int) ([]domain.Event, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	e, err := getEnvironment(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	if err = checkEnvironment(p, e); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,environment_id,generation,kind,payload,created_at FROM heimdall.events WHERE environment_id=$1 AND id>$2 ORDER BY id LIMIT $3`, id, after, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []domain.Event{}
	for rows.Next() {
		var item domain.Event
		if err = rows.Scan(&item.ID, &item.EnvironmentID, &item.Generation, &item.Kind, &item.Payload, &item.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

// bounded deadline for all worker claims; zero or arbitrarily long leases are
// rejected rather than leaving a crashed delivery unclaimable indefinitely.
func validLease(lease time.Duration) bool { return lease >= time.Second && lease <= 15*time.Minute }
