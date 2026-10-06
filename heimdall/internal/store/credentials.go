package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/jackc/pgx/v5"
)

func credentialParts(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "hd" || len(parts[3]) != 43 {
		return "", "", domain.ErrUnauthorized
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return "", "", domain.ErrUnauthorized
	}
	if _, err := uuid.Parse(parts[2]); err != nil {
		return "", "", domain.ErrUnauthorized
	}
	return parts[1], parts[2], nil
}
func newCredential(ctx context.Context, tx pgx.Tx, p domain.Principal, input domain.CredentialInput) (domain.IssuedCredential, error) {
	var issued domain.IssuedCredential
	if input.TTL <= 0 || input.TTL > 30*24*time.Hour {
		return issued, errors.New("credential TTL must be positive and at most 30 days")
	}
	switch input.Kind {
	case "user", "access", "refresh", "enrollment", "ci":
	default:
		return issued, errors.New("invalid credential kind")
	}
	if input.Kind == "user" && (input.Role != "admin" && input.Role != "member" && input.Role != "viewer" || input.ClusterID != "") {
		return issued, errors.New("user credentials require a human role and no cluster scope")
	}
	if input.Kind == "ci" && (input.Role != "ci" || input.ClusterID != "") {
		return issued, errors.New("CI credentials require the CI role and no cluster scope")
	}
	if input.Kind == "access" && input.TTL > 15*time.Minute {
		return issued, errors.New("agent access TTL cannot exceed 15 minutes")
	}
	if input.Kind == "enrollment" && (input.Role != "agent" || input.ClusterID == "" || input.TTL > 10*time.Minute) {
		return issued, errors.New("enrollment must be cluster scoped with TTL at most 10 minutes")
	}
	if (input.Kind == "access" || input.Kind == "refresh") && (input.Role != "agent" || input.ClusterID == "") {
		return issued, errors.New("agent credentials require cluster scope")
	}
	if input.ActorID == "" {
		return issued, errors.New("credential actor is required")
	}
	if err := lockCluster(ctx, tx, input.ClusterID); err != nil {
		return issued, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return issued, err
	}
	id := uuid.NewString()
	token := "hd." + p.TenantID + "." + id + "." + base64.RawURLEncoding.EncodeToString(secret)
	return insertCredential(ctx, tx, p, input, id, token)
}
func lockCluster(ctx context.Context, tx pgx.Tx, id string) error {
	if id == "" {
		return nil
	}
	var found string
	return translate(tx.QueryRow(ctx, `SELECT id::text FROM heimdall.clusters WHERE id=$1 FOR SHARE`, id).Scan(&found))
}
func lockCredentialCluster(ctx context.Context, tx pgx.Tx, id string) error {
	var cluster *string
	if err := tx.QueryRow(ctx, `SELECT cluster_id::text FROM heimdall.credentials WHERE id=$1`, id).Scan(&cluster); err != nil {
		return domain.ErrUnauthorized
	}
	if cluster == nil {
		return domain.ErrUnauthorized
	}
	if err := lockCluster(ctx, tx, *cluster); err != nil {
		return domain.ErrUnauthorized
	}
	return nil
}
func insertCredential(ctx context.Context, tx pgx.Tx, p domain.Principal, input domain.CredentialInput, id, token string) (domain.IssuedCredential, error) {
	var issued domain.IssuedCredential
	hash := sha256.Sum256([]byte(token))
	expires := time.Now().UTC().Add(input.TTL)
	var cluster any
	if input.ClusterID != "" {
		cluster = input.ClusterID
	}
	_, err := tx.Exec(ctx, `INSERT INTO heimdall.credentials(id,tenant_id,actor_id,cluster_id,role,kind,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, p.TenantID, input.ActorID, cluster, input.Role, input.Kind, hash[:], expires)
	if err != nil {
		return issued, translate(err)
	}
	issued = domain.IssuedCredential{Credential: domain.Credential{ID: id, ActorID: input.ActorID, ClusterID: input.ClusterID, Role: input.Role, Kind: input.Kind, ExpiresAt: expires}, Token: token}
	if err = audit(ctx, tx, p, "credential.issue", id, map[string]any{"clusterId": input.ClusterID, "kind": input.Kind, "role": input.Role, "expiresAt": expires}); err != nil {
		return domain.IssuedCredential{}, err
	}
	return issued, nil
}

// A rotation replay never stores bearer material. Successors are derived using
// an operator secret unavailable to a holder of a consumed predecessor token.
// The agent durably saves the nonce before its request and retries that nonce
// if the response or its local commit is lost.
func (s *Store) ExchangeCredentialWithNonce(ctx context.Context, token, expectedKind, clusterID, nonce string) (domain.CredentialPair, error) {
	var pair domain.CredentialPair
	if len(s.credentialKey) < 32 {
		return pair, errors.New("credential derivation key is not configured")
	}
	if len(nonce) < 8 || len(nonce) > 128 || (expectedKind != "enrollment" && expectedKind != "refresh") {
		return pair, domain.ErrUnauthorized
	}
	tenant, id, err := credentialParts(token)
	if err != nil {
		return pair, err
	}
	p := domain.Principal{TenantID: tenant, ActorID: "credential-exchange", Role: "system"}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return pair, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockCredentialCluster(ctx, tx, id); err != nil {
		return pair, err
	}
	var c domain.Credential
	var hash, priorNonce []byte
	var accessID, refreshID *string
	var replayUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,actor_id,COALESCE(cluster_id::text,''),role,kind,token_hash,expires_at,revoked_at,rotation_nonce_hash,successor_access::text,successor_refresh::text,replay_until FROM heimdall.credentials WHERE id=$1 AND expires_at>now() FOR UPDATE`, id).Scan(&c.ID, &c.ActorID, &c.ClusterID, &c.Role, &c.Kind, &hash, &c.ExpiresAt, &c.RevokedAt, &priorNonce, &accessID, &refreshID, &replayUntil)
	candidate := sha256.Sum256([]byte(token))
	if err != nil || subtle.ConstantTimeCompare(hash, candidate[:]) != 1 || c.Kind != expectedKind || c.Role != "agent" || c.ClusterID == "" || (clusterID != "" && clusterID != c.ClusterID) || (expectedKind == "enrollment" && clusterID != c.ClusterID) {
		return pair, domain.ErrUnauthorized
	}
	p.ActorID = c.ActorID
	p.ClusterID = c.ClusterID
	nonceHash := sha256.Sum256([]byte(nonce))
	derive := func(kind string) (string, string) {
		mac := hmac.New(sha256.New, s.credentialKey)
		mac.Write([]byte("heimdall.credential.rotation.v1\x00" + tenant + "\x00" + id + "\x00" + kind + "\x00"))
		mac.Write(nonceHash[:])
		secret := mac.Sum(nil)
		idmac := hmac.New(sha256.New, s.credentialKey)
		idmac.Write([]byte("heimdall.credential.id.v1\x00"))
		idmac.Write(secret)
		idbytes := idmac.Sum(nil)[:16]
		idbytes[6] = (idbytes[6] & 0x0f) | 0x40
		idbytes[8] = (idbytes[8] & 0x3f) | 0x80
		uid := uuid.Must(uuid.FromBytes(idbytes)).String()
		return uid, "hd." + tenant + "." + uid + "." + base64.RawURLEncoding.EncodeToString(secret)
	}
	aID, aToken := derive("access")
	rID, rToken := derive("refresh")
	if c.RevokedAt != nil {
		if replayUntil == nil || !replayUntil.After(time.Now()) || subtle.ConstantTimeCompare(priorNonce, nonceHash[:]) != 1 || accessID == nil || refreshID == nil || *accessID != aID || *refreshID != rID {
			return pair, domain.ErrUnauthorized
		}
		a, err := lookupCredential(ctx, tx, aToken, aID, false)
		if err != nil {
			return pair, err
		}
		r, err := lookupCredential(ctx, tx, rToken, rID, false)
		if err != nil {
			return pair, err
		}
		pair.Access = domain.IssuedCredential{Credential: a, Token: aToken}
		pair.Refresh = domain.IssuedCredential{Credential: r, Token: rToken}
		return pair, nil
	}
	pair.Access, err = insertCredential(ctx, tx, p, domain.CredentialInput{ActorID: c.ActorID, ClusterID: c.ClusterID, Role: "agent", Kind: "access", TTL: 15 * time.Minute}, aID, aToken)
	if err != nil {
		return pair, err
	}
	pair.Refresh, err = insertCredential(ctx, tx, p, domain.CredentialInput{ActorID: c.ActorID, ClusterID: c.ClusterID, Role: "agent", Kind: "refresh", TTL: 30 * 24 * time.Hour}, rID, rToken)
	if err != nil {
		return pair, err
	}
	_, err = tx.Exec(ctx, `UPDATE heimdall.credentials SET revoked_at=now(),rotation_nonce_hash=$2,successor_access=$3,successor_refresh=$4,replay_until=now()+interval '2 minutes' WHERE id=$1`, id, nonceHash[:], aID, rID)
	if err != nil {
		return pair, err
	}
	if err = audit(ctx, tx, p, "credential.consume", id, map[string]any{"kind": expectedKind, "clusterId": c.ClusterID}); err != nil {
		return pair, err
	}
	return pair, tx.Commit(ctx)
}
func (s *Store) IssueCredential(ctx context.Context, p domain.Principal, input domain.CredentialInput) (domain.IssuedCredential, error) {
	if err := requireAdmin(p); err != nil {
		return domain.IssuedCredential{}, err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return domain.IssuedCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockCluster(ctx, tx, input.ClusterID); err != nil {
		return domain.IssuedCredential{}, err
	}
	if input.Kind == "enrollment" {
		if _, err = tx.Exec(ctx, `UPDATE heimdall.credentials SET revoked_at=now() WHERE cluster_id=$1 AND kind='enrollment' AND revoked_at IS NULL`, input.ClusterID); err != nil {
			return domain.IssuedCredential{}, err
		}
	}
	issued, err := newCredential(ctx, tx, p, input)
	if err != nil {
		return issued, err
	}
	return issued, tx.Commit(ctx)
}
func lookupCredential(ctx context.Context, tx pgx.Tx, token, id string, lock bool) (domain.Credential, error) {
	var c domain.Credential
	var hash []byte
	query := `SELECT id::text,actor_id,COALESCE(cluster_id::text,''),role,kind,token_hash,expires_at,revoked_at FROM heimdall.credentials WHERE id=$1 AND expires_at>now() AND revoked_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query, id).Scan(&c.ID, &c.ActorID, &c.ClusterID, &c.Role, &c.Kind, &hash, &c.ExpiresAt, &c.RevokedAt)
	candidate := sha256.Sum256([]byte(token))
	if err != nil || subtle.ConstantTimeCompare(hash, candidate[:]) != 1 {
		return c, domain.ErrUnauthorized
	}
	return c, nil
}

// The tenant component of an opaque token is only a lookup hint: it is never
// trusted as a principal until the complete token hash has matched a live row.
func (s *Store) Authenticate(ctx context.Context, token string) (domain.Principal, error) {
	tenant, id, err := credentialParts(token)
	if err != nil {
		return domain.Principal{}, err
	}
	hint := domain.Principal{TenantID: tenant, ActorID: "authentication", Role: "system"}
	tx, err := s.begin(ctx, hint)
	if err != nil {
		return domain.Principal{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := lookupCredential(ctx, tx, token, id, false)
	if err != nil {
		return domain.Principal{}, err
	}
	if c.Kind != "user" && c.Kind != "access" && c.Kind != "ci" {
		return domain.Principal{}, domain.ErrUnauthorized
	}
	var slug string
	if err = tx.QueryRow(ctx, `SELECT slug FROM heimdall.tenants WHERE id=$1`, tenant).Scan(&slug); err != nil {
		return domain.Principal{}, domain.ErrUnauthorized
	}
	return domain.Principal{TenantID: tenant, TenantSlug: slug, ActorID: c.ActorID, ClusterID: c.ClusterID, Role: c.Role, Kind: c.Kind, CredentialID: c.ID}, nil
}
func (s *Store) ExchangeCredential(ctx context.Context, token, expectedKind, clusterID string) (domain.CredentialPair, error) {
	var pair domain.CredentialPair
	if expectedKind != "enrollment" && expectedKind != "refresh" {
		return pair, domain.ErrUnauthorized
	}
	tenant, id, err := credentialParts(token)
	if err != nil {
		return pair, err
	}
	p := domain.Principal{TenantID: tenant, ActorID: "credential-exchange", Role: "system"}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return pair, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockCredentialCluster(ctx, tx, id); err != nil {
		return pair, err
	}
	c, err := lookupCredential(ctx, tx, token, id, true)
	if err != nil {
		return pair, err
	}
	if c.Kind != expectedKind || c.Role != "agent" || c.ClusterID == "" || (expectedKind == "enrollment" && clusterID != c.ClusterID) || (expectedKind == "refresh" && clusterID != "" && clusterID != c.ClusterID) {
		return pair, domain.ErrUnauthorized
	}
	p.ActorID = c.ActorID
	p.ClusterID = c.ClusterID
	if _, err = tx.Exec(ctx, `UPDATE heimdall.credentials SET revoked_at=now() WHERE id=$1`, id); err != nil {
		return pair, err
	}
	if err = audit(ctx, tx, p, "credential.consume", id, map[string]any{"kind": expectedKind, "clusterId": c.ClusterID}); err != nil {
		return pair, err
	}
	pair.Access, err = newCredential(ctx, tx, p, domain.CredentialInput{ActorID: c.ActorID, ClusterID: c.ClusterID, Role: "agent", Kind: "access", TTL: 15 * time.Minute})
	if err != nil {
		return pair, err
	}
	pair.Refresh, err = newCredential(ctx, tx, p, domain.CredentialInput{ActorID: c.ActorID, ClusterID: c.ClusterID, Role: "agent", Kind: "refresh", TTL: 30 * 24 * time.Hour})
	if err != nil {
		return pair, err
	}
	return pair, tx.Commit(ctx)
}
func (s *Store) RevokeCredential(ctx context.Context, p domain.Principal, id string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE heimdall.credentials SET revoked_at=now(),replay_until=NULL WHERE id=$1 OR id IN (SELECT successor_access FROM heimdall.credentials WHERE id=$1 UNION SELECT successor_refresh FROM heimdall.credentials WHERE id=$1)`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if err = audit(ctx, tx, p, "credential.revoke", id, map[string]any{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RevokeClusterCredentials(ctx context.Context, p domain.Principal, id string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	tx, err := s.begin(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var found string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM heimdall.clusters WHERE id=$1 FOR UPDATE`, id).Scan(&found); err != nil {
		return translate(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE heimdall.credentials SET revoked_at=now() WHERE cluster_id=$1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, p, "cluster.credentials.revoke", id, map[string]any{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
