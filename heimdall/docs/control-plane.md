# Control plane and outbound agent

The control plane stores the desired preview and its durable history. An agent
inside the customer's cluster pulls that intent, runs the existing lifecycle
engine, and reports observations. The API has no Kubernetes client or inbound
connection to the customer's cluster.

## Runtime components

| Process | Responsibility | Authority |
|---|---|---|
| `cmd/api` | Tenant REST API, resumable SSE, credentials, policy, agent channel and verified CI callbacks | Restricted PostgreSQL application role; ephemeral Redis |
| `cmd/orchestrator` | Ordered webhook consumer, canonical GitHub state, commands and comment/check outbox | Same restricted database role; own GitHub App; source SQS queue |
| `cmd/webhook` | Lambda raw-body HMAC verification and FIFO enqueue | Its webhook secret and send-only queue permission |
| `cmd/agent` | Desired-state projection, admission, engine, diagnostics, fixture materialisation and reporting | Cluster-bound token and preview-scoped Kubernetes permissions |
| `cmd/control` | Operator migrations, tenant bootstrap, repository registration and sanitised-data attestation | Separate migration connection for bootstrap; restricted connection for tenant operations |

```mermaid
flowchart LR
    GH[GitHub] --> W[Signed webhook]
    W --> Q[SQS FIFO and DLQ]
    Q --> O[Orchestrator]
    O --> DB[(PostgreSQL intent and history)]
    API[REST and SSE API] --> DB
    CI[Trusted Actions workflow] -->|Image and bundle digests| API
    CI --> R[Customer preview registry]
    A[Customer cluster agent] -->|Outbound desired-state pull| API
    A -->|Fenced observations| API
    A -->|Bundle pull| R
    A --> PE[PreviewEnvironment]
    PE --> E[Controller and lifecycle engine]
    E --> NS[Isolated preview namespaces]
    DB --> O
    O -->|Edited comment and check| GH
```

The [OpenAPI contract](../api/openapi.yaml) is authoritative for request shapes,
roles and routes. Generated clients live in `internal/controlapi/gen`.
Database contracts are generated from `db/queries` with sqlc. Goose migrations
are embedded in the operator executable.

## State, isolation and recovery

Every mutation runs in a transaction with the authenticated tenant set using
`set_config(..., true)`. The application role must be a non-owner without
superuser, `BYPASSRLS`, or privileges that bypass the protected tables. Tables
enforce `FORCE ROW LEVEL SECURITY`; missing tenant context fails closed.
Startup verifies the connection's role instead of trusting the DSN name.

An environment persists across pushes and reopenings. Immutable deployments
record the exact committed spec for each generation. Runtime actions use
compare-and-swap on `version`; callers send the observed version and an
`Idempotency-Key`. Reusing a key with different input is a conflict. Status from
an older generation is rejected and retained as a stale event. Expiry extension
changes metadata/version without reinitialising preview data ([ADR 0005](adr/0005-sources-of-truth-and-generation-fencing.md)).

New pushes reserve a generation while CI builds. The previous committed spec
remains at its original generation; an old image is never relabelled as the new
build. A fresh environment with no committed spec waits for CI. Generation,
current head SHA, trusted workflow identity, installation, repository, cluster,
tenant policy and fixture attestation are rechecked before accepting a build.

Events record stages, smoke results, diagnoses and lifecycle changes. SSE uses
durable event IDs; resume with `Last-Event-ID` or `after`. The stream periodically
reauthenticates, so token revocation ends an existing stream. Pagination is
bounded and uses a keyset cursor. Audit entries and an outbox are committed in
the same transaction as intent changes.

The agent only prunes from a successful authoritative snapshot. An API outage
does not become an empty desired-state list. Fresh tenant policy also controls
admission on every agent replica; stale policy blocks new deployments. Explicit
destruction uses immutable environment identity and ownership checks, so a
stricter policy or unavailable registry cannot prevent cleanup. Preparation
failures remain scoped to one environment.

## Credentials

An administrator creates a cluster and obtains a one-use enrollment token from
`POST /v1/clusters/{clusterID}/enrollment`. The agent exchanges it at
`/v1/agent/register`, then rotates short-lived access/refresh pairs. The API binds
each token to its tenant and cluster. Revoking cluster enrollment invalidates
all its issued tokens immediately.

The agent stores a random exchange nonce **before** a register/refresh request,
using resource-version compare-and-swap in its pre-created authentication
Secret. The database keeps credential hashes, not plaintext bearer tokens.
An operator-provided derivation key makes a lost response replayable with the
same predecessor and nonce for two minutes. Replicas share this durable state;
they do not concurrently consume separate successors.

If an exchange response and the local write are both lost for longer than the
two-minute window, re-enroll deliberately: revoke old cluster credentials,
issue a new enrollment, replace the Secret's `enrollment` key, and restart the
agent. A new enrollment resets the old session. Retrying the same enrollment
preserves its saved nonce. Keep the derivation key stable across API replicas
and restarts; rotate it only through a coordinated credential re-enrollment.

## Logs and fixture data

Logs are requested explicitly through `/v1/environments/{id}/logs`. The leader
polls `/v1/agent/log-requests`, reads only declared application workloads in the
current generation, resolves referenced secrets, redacts, and posts a bounded
tail. Requests/results expire after two minutes. Redis shares these ephemeral
results across API replicas; neither log bodies nor fixture bytes enter
PostgreSQL, audit records, GitHub comments or continuous central logs.

Run Redis with **RDB and AOF disabled**, no persistent volume, bounded memory
and `noeviction`. The API verifies persistence settings with `CONFIG GET`
at startup and readiness; its Redis ACL therefore needs that read-only command
as well as the broker's key/Lua operations. Use a dedicated Redis database and
TLS/access controls in a hosted deployment. A provider that prevents verifying
these settings is unsuitable for this log broker. `--local` permits the bounded
in-memory broker only for single-process development.

CI packs only the declared configuration and optional SQL fixture into a
reproducible OCI artifact in the customer's registry. The agent verifies the
manifest, layer hashes, exact configuration hash and declared fixture path.
SQL imports require an unexpired operator attestation for the exact config and
sanitised fixture hashes. It materialises immutable, reserved ConfigMaps in the
agent namespace, binds their lifetime to the exact PreviewEnvironment UID, and
prunes obsolete imports after successful convergence or explicit destruction.
An ownerless crash leftover needs a fresh authoritative read, no references and
one hour of grace before removal. The central API receives metadata only.

## Local startup on Windows

Run from the `heimdall` directory with Docker Desktop, Go and PowerShell.
The provided Compose stack binds the API to localhost and uses clearly local
database passwords. Use separate secret storage, TLS and managed connections
when hosting it.

```powershell
./deploy/local/prepare.ps1
docker compose -f deploy/local/compose.yaml -f deploy/local/compose.operator.yaml up -d postgres redis
docker compose -f deploy/local/compose.yaml --profile setup run --rm migrate
```

The operator override exposes PostgreSQL **only on localhost** for host-side
provisioning. A private forwarded connection also works. Set the separate
operator and application DSNs:

```powershell
$env:HEIMDALL_MIGRATION_DATABASE_URL = 'postgres://heimdall_owner:local-operator-only@localhost:5432/heimdall?sslmode=disable'
$env:HEIMDALL_DATABASE_URL = 'postgres://heimdall_app:local-app-only@localhost:5432/heimdall?sslmode=disable'
$env:HEIMDALL_CREDENTIAL_KEY_FILE = (Resolve-Path out/control-local/credential-key).Path
go run ./cmd/control provision --input config/operator-provision.example.json --output out/control-local/admin.json
docker compose -f deploy/local/compose.yaml up -d api
```

Copy and edit the example first: choose the tenant UUID/slug, explicit registry
prefix and quota. Policy JSON uses the capitalised field names in OpenAPI;
durations are integer nanoseconds. Operator results go to a **new** private
file; existing files are refused. On Linux, restrict secret directories and make
only the service identity able to read mounted files (the image UID is 65532).
Do not use the migration role in API/worker configuration.

Create and enroll the customer cluster using the issued administrator token.
Read it from the private output file; do not put it in command arguments or
source-controlled values. For example:

```powershell
$admin = Get-Content out/control-local/admin.json -Raw | ConvertFrom-Json
$headers = @{ Authorization = 'Bearer ' + $admin.token }
$cluster = Invoke-RestMethod http://localhost:8080/v1/clusters -Method Post -Headers $headers -ContentType application/json -Body '{"name":"local-kind","tier":1}'
$enrollment = Invoke-RestMethod "http://localhost:8080/v1/clusters/$($cluster.id)/enrollment" -Method Post -Headers $headers
[IO.File]::WriteAllText((Join-Path (Get-Location) 'out/control-local/enrollment'), $enrollment.token)
kubectl create namespace heimdall-system
kubectl -n heimdall-system create secret generic heimdall-agent-auth --from-file=enrollment=out/control-local/enrollment
```

Install the chart with `source.type=api`, `source.controlPlane.clusterID` from
that response, the exact API origin, a digest-pinned agent image and the target
cluster's platform settings. Local kind can reach the host API through
`http://host.docker.internal:8080`; set `allowLocalHTTP=true` explicitly for that
local setup. Normal installations require HTTPS. Enable `registryPlainHTTP`
only for a disposable local registry. Keep two replicas for credential/leader
failover. The chart grants narrowly scoped access to the pre-created auth Secret.

Register the own App/repository using `cmd/control repository` and the edited
[repository input](../config/operator-repository.example.json). Substitute the
actual installation ID, cluster ID, tenant UUID, repository UUID and reviewed
workflow commit. The App permissions, HTTPS endpoints, registry role and live
acceptance procedure are in [github-app-setup.md](github-app-setup.md).

With the operator/application DSNs and credential-key file still configured,
copy `config/operator-repository.example.json` to
`out/control-local/repository-input.json`, replace its placeholder values, and
register the edited input using a new output file:

```powershell
go run ./cmd/control repository --input out/control-local/repository-input.json --output out/control-local/repository-result.json
```

The customer push-role template is
[infra/registry/preview-role.yaml](../infra/registry/preview-role.yaml). Give the
cluster agent a separate read-only identity using
[agent-pull-policy.json](../infra/registry/agent-pull-policy.json), scoped to the
same preview repositories. For EKS IRSA, attach that role to the chart's agent
ServiceAccount through `serviceAccount.annotations.eks.amazonaws.com/role-arn`
and configure its trust for the exact namespace/ServiceAccount. This identity
must not be attached to application preview workloads.

For local GitHub ingress/consumption, supply `HEIMDALL_GITHUB_APP_ID`,
`HEIMDALL_GITHUB_PRIVATE_KEY_PATH`, `HEIMDALL_GITHUB_WEBHOOK_SECRET_PATH` and
`HEIMDALL_OIDC_AUDIENCE` from operator configuration, then start:

```powershell
docker compose -f deploy/local/compose.yaml -f deploy/local/compose.github.yaml --profile github up -d
```

Expose the API through a reachable HTTPS ingress. The local overlay uses dummy
credentials only for LocalStack SQS. It still uses the **real own App** for
GitHub and requires genuine Actions OIDC callbacks. In AWS, replace LocalStack
with the provided webhook/FIFO stack and use workload identity instead.

## Verification and operations

`make test-integration` requires real PostgreSQL, Redis and LocalStack containers.
`make e2e-control` drives all preview mutations through the API against kind and
checks namespaces, stage history, two-replica recovery, redacted logs and
cleanup. The active workflow is `../.github/workflows/ci.yml` at the Git root;
the nested historical workflow is not an active GitHub workflow in this checkout.

Back up PostgreSQL and test restores with its migration version. Keep webhook
delivery/audit/deployment history after preview deletion. Monitor API readiness,
agent heartbeat age, FIFO DLQ depth, and dead inbox/outbox attempts. Redrive only
after correcting the cause. Expired credentials need a new operator-issued
token; revoked installations must be disabled in repository registration.
Production network isolation, gateway authentication, cloud security validation
and full reliability instrumentation remain the dedicated P7–P10 phases.
