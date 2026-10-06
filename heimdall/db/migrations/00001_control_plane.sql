-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'heimdall_app') THEN
    CREATE ROLE heimdall_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
  END IF;
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'heimdall_app' AND (rolsuper OR rolbypassrls OR rolcreaterole)) THEN
    RAISE EXCEPTION 'heimdall_app must be a non-privileged, non-owner application role';
  END IF;
END $$;
-- +goose StatementEnd
CREATE SCHEMA heimdall;
REVOKE ALL ON SCHEMA heimdall FROM PUBLIC;
GRANT USAGE ON SCHEMA heimdall TO heimdall_app;

CREATE TABLE heimdall.tenants (
  id uuid PRIMARY KEY, slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  name text NOT NULL CHECK(length(name) BETWEEN 1 AND 200), created_at timestamptz NOT NULL DEFAULT now()
);
-- Only routing identifiers are outside RLS. They contain no tenant data and are
-- readable only through narrow functions. The application cannot mutate them.
CREATE TABLE heimdall.tenant_routes (tenant_id uuid PRIMARY KEY REFERENCES heimdall.tenants(id));
CREATE TABLE heimdall.installation_routes (installation_id bigint PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id));

CREATE TABLE heimdall.tenant_policy (
  tenant_id uuid PRIMARY KEY REFERENCES heimdall.tenants(id), policy jsonb NOT NULL CHECK(jsonb_typeof(policy) = 'object'),
  version bigint NOT NULL DEFAULT 1 CHECK(version > 0), desired_revision bigint NOT NULL DEFAULT 1 CHECK(desired_revision > 0), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE heimdall.clusters (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), name text NOT NULL CHECK(length(name) BETWEEN 1 AND 63),
  last_heartbeat timestamptz, agent_version text NOT NULL DEFAULT '', tier integer NOT NULL DEFAULT 1 CHECK(tier IN (0,1)), created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(tenant_id,id), UNIQUE(tenant_id,name)
);
CREATE TABLE heimdall.installations (
  id bigint PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), account text NOT NULL,
  suspended boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,id)
);
CREATE TABLE heimdall.repositories (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), github_id bigint NOT NULL UNIQUE,
  installation_id bigint NOT NULL, cluster_id uuid NOT NULL, full_name text NOT NULL, default_branch text NOT NULL DEFAULT 'main',
  enabled boolean NOT NULL DEFAULT true, trusted_workflow_ref text NOT NULL DEFAULT '', trusted_workflow_sha text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,id),
  FOREIGN KEY(tenant_id,installation_id) REFERENCES heimdall.installations(tenant_id,id),
  FOREIGN KEY(tenant_id,cluster_id) REFERENCES heimdall.clusters(tenant_id,id)
);
CREATE TABLE heimdall.pull_requests (
  tenant_id uuid NOT NULL, repository_id uuid NOT NULL, number bigint NOT NULL CHECK(number > 0), head_sha text NOT NULL,
  base_sha text NOT NULL DEFAULT '', state text NOT NULL CHECK(state IN ('open','closed','refused')),
  is_fork boolean NOT NULL DEFAULT false, needs_approval boolean NOT NULL DEFAULT false, approved_sha text NOT NULL DEFAULT '',
  config_digest text NOT NULL DEFAULT '', baseline_digest text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(tenant_id,repository_id,number), FOREIGN KEY(tenant_id,repository_id) REFERENCES heimdall.repositories(tenant_id,id)
);
CREATE TABLE heimdall.environments (
  id text PRIMARY KEY CHECK(id ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(id) <= 63), tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id),
  repository_id uuid NOT NULL, cluster_id uuid NOT NULL, name text NOT NULL, pull_request bigint NOT NULL CHECK(pull_request > 0),
  owner text NOT NULL, commit_sha text NOT NULL, version bigint NOT NULL DEFAULT 1 CHECK(version > 0), generation bigint NOT NULL DEFAULT 1 CHECK(generation > 0),
  reset_nonce bigint NOT NULL DEFAULT 0 CHECK(reset_nonce >= 0), desired_state text NOT NULL CHECK(desired_state IN ('Running','Destroyed')),
  phase text NOT NULL CHECK(phase IN ('Pending','Queued','Provisioning','Ready','Degraded','Resetting','Failed','Destroying','Destroyed')),
  build_state text NOT NULL DEFAULT 'ready' CHECK(build_state IN ('pending','ready','refused')),
  spec jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(spec) = 'object' AND octet_length(spec::text) <= 524288),
  status jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(status) = 'object' AND octet_length(status::text) <= 32768),
  expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(tenant_id,id), UNIQUE(tenant_id,repository_id,pull_request),
  FOREIGN KEY(tenant_id,repository_id) REFERENCES heimdall.repositories(tenant_id,id),
  FOREIGN KEY(tenant_id,cluster_id) REFERENCES heimdall.clusters(tenant_id,id)
);
CREATE INDEX environments_cluster ON heimdall.environments(tenant_id,cluster_id,id);
CREATE TABLE heimdall.deployments (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, environment_id text NOT NULL, generation bigint NOT NULL CHECK(generation > 0),
  commit_sha text NOT NULL, spec jsonb NOT NULL CHECK(jsonb_typeof(spec) = 'object' AND octet_length(spec::text) <= 524288),
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,environment_id,generation),
  FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);
CREATE TABLE heimdall.events (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL, environment_id text NOT NULL, generation bigint NOT NULL,
  kind text NOT NULL, payload jsonb NOT NULL DEFAULT '{}' CHECK(octet_length(payload::text) <= 32768),
  source_event_id text, created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id), UNIQUE(tenant_id,environment_id,source_event_id)
);
CREATE INDEX events_timeline ON heimdall.events(tenant_id,environment_id,id);
CREATE TABLE heimdall.diagnoses (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL, environment_id text NOT NULL, generation bigint NOT NULL,
  code text NOT NULL CHECK(length(code) <= 64), summary text NOT NULL CHECK(length(summary) <= 1024), suggestion text NOT NULL CHECK(length(suggestion) <= 1024),
  evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence) = 'array' AND jsonb_array_length(evidence) <= 10 AND octet_length(evidence::text) <= 4096),
  subject text NOT NULL DEFAULT '' CHECK(length(subject) <= 253), stage text NOT NULL DEFAULT '' CHECK(length(stage) <= 64), created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);
CREATE TABLE heimdall.smoke_runs (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL, environment_id text NOT NULL, generation bigint NOT NULL,
  name text NOT NULL CHECK(length(name) <= 200), passed boolean NOT NULL, duration_ms bigint NOT NULL CHECK(duration_ms >= 0),
  summary text NOT NULL CHECK(length(summary) <= 1024), created_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);
CREATE TABLE heimdall.quotas (
  tenant_id uuid PRIMARY KEY REFERENCES heimdall.tenants(id), max_environments integer NOT NULL CHECK(max_environments > 0),
  max_cpu_milli integer NOT NULL CHECK(max_cpu_milli > 0), max_memory_mi integer NOT NULL CHECK(max_memory_mi > 0), max_storage_mi integer NOT NULL CHECK(max_storage_mi > 0)
);
CREATE TABLE heimdall.usage_samples (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL, environment_id text NOT NULL,
  cpu_milli integer NOT NULL CHECK(cpu_milli >= 0), memory_mi integer NOT NULL CHECK(memory_mi >= 0), storage_mi integer NOT NULL CHECK(storage_mi >= 0),
  sampled_at timestamptz NOT NULL, FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);
CREATE TABLE heimdall.audit_log (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), actor_id text NOT NULL, action text NOT NULL, resource text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}' CHECK(octet_length(metadata::text) <= 16384), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE heimdall.credentials (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), actor_id text NOT NULL, cluster_id uuid,
  role text NOT NULL CHECK(role IN ('admin','member','agent','ci','system')), kind text NOT NULL CHECK(kind IN ('user','access','refresh','enrollment','ci')),
  token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash) = 32), expires_at timestamptz NOT NULL, revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY(tenant_id,cluster_id) REFERENCES heimdall.clusters(tenant_id,id)
);
CREATE INDEX credentials_cluster ON heimdall.credentials(tenant_id,cluster_id);
CREATE TABLE heimdall.idempotency_keys (
  tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), actor_id text NOT NULL, key text NOT NULL CHECK(length(key) BETWEEN 1 AND 200),
  request_hash bytea NOT NULL CHECK(octet_length(request_hash) = 32), response jsonb, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(tenant_id,actor_id,key)
);
CREATE TABLE heimdall.webhook_deliveries (
  delivery_id text PRIMARY KEY CHECK(length(delivery_id) BETWEEN 1 AND 200), tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), event text NOT NULL,
  repository_id uuid, pull_request bigint NOT NULL DEFAULT 0, payload jsonb NOT NULL CHECK(octet_length(payload::text) <= 1048576),
  payload_hash text NOT NULL CHECK(length(payload_hash) = 64), state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','complete','dead')),
  attempts integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(), lease_token text, lease_until timestamptz, worker text,
  last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 1024), received_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz,
  FOREIGN KEY(tenant_id,repository_id) REFERENCES heimdall.repositories(tenant_id,id)
);
CREATE INDEX deliveries_work ON heimdall.webhook_deliveries(tenant_id,available_at,received_at) WHERE state IN ('pending','processing');
CREATE TABLE heimdall.outbox (
  id bigserial PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES heimdall.tenants(id), environment_id text, generation bigint NOT NULL DEFAULT 0,
  kind text NOT NULL, payload jsonb NOT NULL DEFAULT '{}' CHECK(octet_length(payload::text) <= 32768), state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','complete','dead')),
  attempts integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(), lease_token text, lease_until timestamptz, worker text,
  last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 1024), created_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz,
  FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);
CREATE INDEX outbox_work ON heimdall.outbox(tenant_id,available_at,id) WHERE state IN ('pending','processing');
CREATE TABLE heimdall.github_delivery_state (
  tenant_id uuid NOT NULL, environment_id text NOT NULL, generation bigint NOT NULL, comment_id bigint NOT NULL DEFAULT 0, check_id bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(tenant_id,environment_id), FOREIGN KEY(tenant_id,environment_id) REFERENCES heimdall.environments(tenant_id,id)
);

-- +goose StatementBegin
DO $$ DECLARE item text; col text; BEGIN
  FOREACH item IN ARRAY ARRAY['tenants','tenant_policy','clusters','installations','repositories','pull_requests','environments','deployments','events','diagnoses','smoke_runs','quotas','usage_samples','audit_log','credentials','idempotency_keys','webhook_deliveries','outbox','github_delivery_state'] LOOP
    col := CASE WHEN item = 'tenants' THEN 'id' ELSE 'tenant_id' END;
    EXECUTE format('ALTER TABLE heimdall.%I ENABLE ROW LEVEL SECURITY', item);
    EXECUTE format('ALTER TABLE heimdall.%I FORCE ROW LEVEL SECURITY', item);
    EXECUTE format('CREATE POLICY tenant_boundary ON heimdall.%I USING (%I::text = nullif(current_setting(''app.tenant_id'', true), '''')) WITH CHECK (%I::text = nullif(current_setting(''app.tenant_id'', true), ''''))', item, col, col);
  END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA heimdall TO heimdall_app;
REVOKE ALL ON heimdall.tenant_routes, heimdall.installation_routes FROM heimdall_app;
REVOKE INSERT, UPDATE ON heimdall.tenants FROM heimdall_app;
REVOKE UPDATE ON heimdall.deployments, heimdall.events, heimdall.audit_log, heimdall.usage_samples FROM heimdall_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA heimdall TO heimdall_app;
-- +goose StatementBegin
CREATE FUNCTION heimdall.resolve_installation(target bigint) RETURNS uuid
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, heimdall
  AS $$ SELECT tenant_id FROM heimdall.installation_routes WHERE installation_id = target $$;
CREATE FUNCTION heimdall.active_tenants() RETURNS SETOF uuid
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, heimdall
  AS $$ SELECT tenant_id FROM heimdall.tenant_routes ORDER BY tenant_id $$;
CREATE FUNCTION heimdall.route_installation() RETURNS trigger
  LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, heimdall
  AS $$ BEGIN
    INSERT INTO heimdall.installation_routes(installation_id,tenant_id) VALUES(NEW.id,NEW.tenant_id)
      ON CONFLICT(installation_id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id;
    RETURN NEW;
  END $$;
CREATE TRIGGER installation_route AFTER INSERT ON heimdall.installations FOR EACH ROW EXECUTE FUNCTION heimdall.route_installation();
CREATE FUNCTION heimdall.route_tenant() RETURNS trigger
  LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, heimdall
  AS $$ BEGIN INSERT INTO heimdall.tenant_routes(tenant_id) VALUES(NEW.id); RETURN NEW; END $$;
CREATE TRIGGER tenant_route AFTER INSERT ON heimdall.tenants FOR EACH ROW EXECUTE FUNCTION heimdall.route_tenant();
CREATE FUNCTION heimdall.immutable_history() RETURNS trigger LANGUAGE plpgsql
  AS $$ BEGIN RAISE EXCEPTION 'deployment and audit history are append-only'; END $$;
CREATE TRIGGER immutable_deployment BEFORE UPDATE OR DELETE ON heimdall.deployments FOR EACH ROW EXECUTE FUNCTION heimdall.immutable_history();
CREATE TRIGGER immutable_audit BEFORE UPDATE OR DELETE ON heimdall.audit_log FOR EACH ROW EXECUTE FUNCTION heimdall.immutable_history();
-- +goose StatementEnd
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA heimdall FROM PUBLIC;
GRANT EXECUTE ON FUNCTION heimdall.resolve_installation(bigint), heimdall.active_tenants() TO heimdall_app;

-- +goose Down
DROP SCHEMA heimdall CASCADE;
-- The operator-managed LOGIN role is intentionally retained, including its
-- externally provisioned password. Rolling a migration back cannot erase it.
