package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/domain"
	queries "github.com/heimdall-dev/heimdall/internal/store/sql"
	"github.com/jackc/pgx/v5"
)

func (s *Store) GetTenant(ctx context.Context, p domain.Principal) (domain.Tenant, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Tenant{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := queries.New(tx).GetTenant(ctx, dbUUID(p.TenantID))
	return domain.Tenant{ID: t.ID, Slug: t.Slug, Name: t.Name}, translate(err)
}
func (s *Store) GetPolicy(ctx context.Context, p domain.Principal) (config.Policy, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return config.Policy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return getPolicy(ctx, tx, p)
}
func getPolicy(ctx context.Context, tx pgx.Tx, p domain.Principal) (config.Policy, error) {
	var policy config.Policy
	data, err := queries.New(tx).GetPolicy(ctx, dbUUID(p.TenantID))
	if err != nil {
		return policy, translate(err)
	}
	err = json.Unmarshal(data, &policy)
	return policy, err
}
func (s *Store) SetPolicy(ctx context.Context, p domain.Principal, policy config.Policy) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if policy.AllowedSecrets == nil || policy.AllowedRegistries == nil {
		return errors.New("production policy must explicitly list allowed secrets and registries")
	}
	if policy.MaxTTL <= 0 || policy.MinTTL <= 0 || policy.MaxTTL < policy.MinTTL || policy.MaxTotalCPUMilli <= 0 || policy.MaxTotalMemoryMi <= 0 {
		return errors.New("policy requires positive resource and TTL ceilings")
	}
	switch policy.MaxVisibility {
	case config.VisibilityPrivate, config.VisibilityOrg, config.VisibilityPublic:
	default:
		return errors.New("policy has invalid visibility")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE heimdall.tenant_policy SET policy=$2,version=version+1,desired_revision=desired_revision+1,updated_at=now() WHERE tenant_id=$1`, p.TenantID, raw(policy))
	if err != nil {
		return err
	}
	if err = audit(ctx, tx, p, "policy.update", p.TenantID, policy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func scanCluster(row pgx.Row) (domain.Cluster, error) {
	var c domain.Cluster
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.LastHeartbeat, &c.AgentVersion, &c.Tier, &c.CreatedAt)
	return c, translate(err)
}

const clusterColumns = `id::text,tenant_id::text,name,last_heartbeat,agent_version,tier,created_at`

func (s *Store) CreateCluster(ctx context.Context, p domain.Principal, c domain.Cluster) (domain.Cluster, error) {
	if err := requireAdmin(p); err != nil {
		return c, err
	}
	if c.Tier != 0 && c.Tier != 1 {
		return c, errors.New("only trusted single-tenant or customer-owned clusters are supported")
	}
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.TenantID != "" && c.TenantID != p.TenantID {
		return c, domain.ErrForbidden
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err = scanCluster(tx.QueryRow(ctx, `INSERT INTO heimdall.clusters(id,tenant_id,name,tier) VALUES($1,$2,$3,$4) RETURNING `+clusterColumns, c.ID, p.TenantID, c.Name, c.Tier))
	if err != nil {
		return c, err
	}
	if err = audit(ctx, tx, p, "cluster.create", c.ID, map[string]any{"name": c.Name, "tier": c.Tier}); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s *Store) GetCluster(ctx context.Context, p domain.Principal, id string) (domain.Cluster, error) {
	if p.Role == "agent" && p.ClusterID != id {
		return domain.Cluster{}, domain.ErrNotFound
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Cluster{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return scanCluster(tx.QueryRow(ctx, `SELECT `+clusterColumns+` FROM heimdall.clusters WHERE id=$1`, id))
}
func (s *Store) ListClusters(ctx context.Context, p domain.Principal) ([]domain.Cluster, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT `+clusterColumns+` FROM heimdall.clusters WHERE ($1='' OR id::text=$1) ORDER BY id`, p.ClusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []domain.Cluster{}
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}
func (s *Store) Heartbeat(ctx context.Context, p domain.Principal, agentVersion string) (domain.Cluster, error) {
	if p.Role != "agent" || p.ClusterID == "" {
		return domain.Cluster{}, domain.ErrForbidden
	}
	if len(agentVersion) > 128 {
		return domain.Cluster{}, errors.New("agent version exceeds limit")
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Cluster{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanCluster(tx.QueryRow(ctx, `UPDATE heimdall.clusters SET last_heartbeat=now(),agent_version=$2 WHERE id=$1 RETURNING `+clusterColumns, p.ClusterID, agentVersion))
	if err != nil {
		return c, err
	}
	if err = audit(ctx, tx, p, "cluster.heartbeat", c.ID, map[string]any{"agentVersion": agentVersion}); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s *Store) PutInstallation(ctx context.Context, p domain.Principal, i domain.Installation) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if i.TenantID != "" && i.TenantID != p.TenantID {
		return domain.ErrForbidden
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO heimdall.installations(id,tenant_id,account,suspended) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET account=EXCLUDED.account,suspended=EXCLUDED.suspended`, i.ID, p.TenantID, i.Account, i.Suspended)
	if err != nil {
		return translate(err)
	}
	if err = audit(ctx, tx, p, "installation.upsert", fmt.Sprint(i.ID), map[string]any{"account": i.Account, "suspended": i.Suspended}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) PutRepository(ctx context.Context, p domain.Principal, r domain.Repository) (domain.Repository, error) {
	if err := requireAdmin(p); err != nil {
		return r, err
	}
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.TenantID != "" && r.TenantID != p.TenantID {
		return r, domain.ErrForbidden
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO heimdall.repositories(id,tenant_id,github_id,installation_id,cluster_id,full_name,default_branch,enabled,trusted_workflow_ref,trusted_workflow_sha) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(id) DO UPDATE SET enabled=EXCLUDED.enabled,default_branch=EXCLUDED.default_branch,trusted_workflow_ref=EXCLUDED.trusted_workflow_ref,trusted_workflow_sha=EXCLUDED.trusted_workflow_sha WHERE heimdall.repositories.github_id=EXCLUDED.github_id AND heimdall.repositories.cluster_id=EXCLUDED.cluster_id AND heimdall.repositories.installation_id=EXCLUDED.installation_id`, r.ID, p.TenantID, r.GitHubID, r.InstallationID, r.ClusterID, r.FullName, r.DefaultBranch, r.Enabled, r.TrustedWorkflowRef, r.TrustedWorkflowSHA)
	if err != nil {
		return r, translate(err)
	}
	r.TenantID = p.TenantID
	if err = audit(ctx, tx, p, "repository.upsert", r.ID, r); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

const repositoryColumns = `id::text,tenant_id::text,github_id,installation_id,cluster_id::text,full_name,default_branch,enabled,trusted_workflow_ref,trusted_workflow_sha`

func scanRepository(row pgx.Row) (domain.Repository, error) {
	var r domain.Repository
	err := row.Scan(&r.ID, &r.TenantID, &r.GitHubID, &r.InstallationID, &r.ClusterID, &r.FullName, &r.DefaultBranch, &r.Enabled, &r.TrustedWorkflowRef, &r.TrustedWorkflowSHA)
	return r, translate(err)
}
func (s *Store) GetRepository(ctx context.Context, p domain.Principal, id string) (domain.Repository, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Repository{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return scanRepository(tx.QueryRow(ctx, `SELECT `+repositoryColumns+` FROM heimdall.repositories WHERE id=$1`, id))
}
func (s *Store) RepositoryByGitHubID(ctx context.Context, p domain.Principal, id int64) (domain.Repository, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Repository{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return scanRepository(tx.QueryRow(ctx, `SELECT `+repositoryColumns+` FROM heimdall.repositories WHERE github_id=$1`, id))
}
func (s *Store) Snapshot(ctx context.Context, p domain.Principal) (domain.DesiredSnapshot, error) {
	var snap domain.DesiredSnapshot
	if p.Role != "agent" || p.ClusterID == "" {
		return snap, domain.ErrForbidden
	}
	tx, err := s.beginOptions(ctx, p, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snap, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	snap.Policy, err = getPolicy(ctx, tx, p)
	if err != nil {
		return snap, err
	}
	if err = tx.QueryRow(ctx, `SELECT desired_revision FROM heimdall.tenant_policy WHERE tenant_id=$1`, p.TenantID).Scan(&snap.Revision); err != nil {
		return snap, err
	}
	rows, err := tx.Query(ctx, `SELECT `+environmentColumns+` FROM heimdall.environments WHERE cluster_id=$1 AND phase<>'Destroyed' AND spec<>'{}'::jsonb ORDER BY id`, p.ClusterID)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	snap.Environments = []domain.Environment{}
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return snap, err
		}
		snap.Environments = append(snap.Environments, e)
	}
	return snap, rows.Err()
}
func (s *Store) Desired(ctx context.Context, p domain.Principal) ([]domain.Environment, error) {
	snap, err := s.Snapshot(ctx, p)
	return snap.Environments, err
}
func (s *Store) DesiredRevision(ctx context.Context, p domain.Principal) (int64, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var value int64
	err = tx.QueryRow(ctx, `SELECT desired_revision FROM heimdall.tenant_policy WHERE tenant_id=$1`, p.TenantID).Scan(&value)
	return value, translate(err)
}
func (s *Store) GetQuota(ctx context.Context, p domain.Principal) (domain.Quota, error) {
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.Quota{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var q domain.Quota
	err = tx.QueryRow(ctx, `SELECT max_environments,max_cpu_milli,max_memory_mi,max_storage_mi FROM heimdall.quotas WHERE tenant_id=$1`, p.TenantID).Scan(&q.MaxEnvironments, &q.MaxCPUMilli, &q.MaxMemoryMi, &q.MaxStorageMi)
	return q, translate(err)
}
func (s *Store) SetQuota(ctx context.Context, p domain.Principal, q domain.Quota) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE heimdall.quotas SET max_environments=$2,max_cpu_milli=$3,max_memory_mi=$4,max_storage_mi=$5 WHERE tenant_id=$1`, p.TenantID, q.MaxEnvironments, q.MaxCPUMilli, q.MaxMemoryMi, q.MaxStorageMi)
	if err != nil {
		return err
	}
	if err = audit(ctx, tx, p, "quota.update", p.TenantID, q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
