-- name: GetTenant :one
SELECT id::text, slug, name FROM heimdall.tenants WHERE id = $1;

-- name: GetEnvironment :one
SELECT id,tenant_id::text,repository_id::text,cluster_id::text,name,pull_request,owner,commit_sha,version,generation,reset_nonce,desired_state,phase,build_state,spec,status,expires_at,created_at,updated_at FROM heimdall.environments WHERE id = $1;

-- name: LockEnvironment :one
SELECT id,tenant_id::text,repository_id::text,cluster_id::text,name,pull_request,owner,commit_sha,version,generation,reset_nonce,desired_state,phase,build_state,spec,status,expires_at,created_at,updated_at FROM heimdall.environments WHERE id = $1 FOR UPDATE;

-- name: Timeline :many
SELECT id,environment_id,generation,kind,payload,created_at FROM heimdall.events WHERE environment_id = $1 AND id > $2 ORDER BY id LIMIT $3;

-- name: InsertAudit :exec
INSERT INTO heimdall.audit_log(tenant_id,actor_id,action,resource,metadata) VALUES($1,$2,$3,$4,$5);

-- name: InsertEvent :exec
INSERT INTO heimdall.events(tenant_id,environment_id,generation,kind,payload,source_event_id) VALUES($1,$2,$3,$4,$5,$6);

-- name: InsertOutbox :exec
INSERT INTO heimdall.outbox(tenant_id,environment_id,generation,kind,payload) VALUES($1,$2,$3,$4,$5);

-- name: GetPolicy :one
SELECT policy FROM heimdall.tenant_policy WHERE tenant_id = $1;

-- name: CompareAndSwapStatus :execrows
UPDATE heimdall.environments SET phase = $1, status = $2, version = version + 1, updated_at = now() WHERE id = $3 AND version = $4 AND generation = $5;

-- name: LookupCredential :one
SELECT id::text,actor_id,COALESCE(cluster_id::text,'') AS cluster_id,role,kind,token_hash,expires_at,revoked_at FROM heimdall.credentials WHERE id = $1;
