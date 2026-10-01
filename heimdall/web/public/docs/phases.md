# Heimdall: phases and implementation plan (revision 2)

Each phase ends in something **runnable and demoable on its own**, depends only
on earlier phases, and has explicit exit criteria. Do not start phase N+1 until
N's exit criteria pass in CI.

Revision 2 incorporates an architecture review. The binding decisions are ADRs
[0004](adr/0004-tenant-isolation-and-cluster-ownership.md) (isolation and cluster
ownership), [0005](adr/0005-sources-of-truth-and-generation-fencing.md) (sources
of truth), [0006](adr/0006-preview-config-trust-policy.md) (config trust) and
[0007](adr/0007-resource-ownership.md) (who owns which Kubernetes objects).
The [revision log](#revision-2-what-changed-and-why) at the end lists every change.

```text
P0 scaffold+config ─> P0.1 trust policy ─> P1 render+secure defaults ─> P2 engine+local CLI
                                                                              │
                                              P4 diagnostics (fixtures) ◄─────┤
                                                                              ▼
                                                           P3 agent + controller + CRD
                                                                              │
P5 control plane (state, API, audit, fencing) ◄───────────────────────────────┘
        └─> P6 GitHub + secure CI contract ─> P7 gateway/isolation/secrets ─> P8 lifecycle/cost
                                                         └────────────┬────────────────┘
                                              P9 AWS (EKS validation) ─> P10 reliability + real-EKS security
                                                                              └─> P11 dashboard + launch demo
```

**P7 must finish before any preview URL is exposed beyond a trusted team.**

## Operating modes (cost reality)

The monthly budget is about $100. EKS control plane alone is roughly $73/month
before nodes, NAT, load balancers, RDS and logs, so a permanent production-style
AWS stack is out of reach. Work in three modes:

| Mode | Where | Used for |
|---|---|---|
| **Local** | Docker Desktop + kind + LocalStack + docker-compose Postgres | Daily development, all CI |
| **Demo AWS** | Short-lived EKS + RDS, created and destroyed by script around a demo or test run | AWS/EKS validation, security tests, portfolio showcase |
| **Customer** | Customer's own EKS and AWS account running the agent (ADR 0004) | The real product model |

Every phase is verifiable in *Local* mode unless it says otherwise.

## Cross-cutting rules (apply to every phase)

| Topic | Rule |
|---|---|
| Trust | PRs are hostile input. Policy comes from outside the PR; the PR can only narrow it (ADR 0006). Dangerous knobs are not expressible in `heimdall.yaml`. |
| Truth | One owner per fact (ADR 0005). Labels are metadata only. Every deployment carries a monotonically increasing `generation`; stale work cannot change current state. |
| Ownership | The agent/controller owns everything inside preview namespaces; Helm only ships Heimdall's own components; no Helm hooks (ADR 0007). |
| Testing pyramid | Unit + table tests → golden files (rendered objects, PR comments) → `envtest` → kind e2e → LocalStack (SQS/S3) → **real-EKS security tests (P9/P10)**. Kind cannot prove EKS network/IMDS isolation. |
| Idempotency | Every cluster, queue and DB operation is safe to run twice; deterministic names; server-side apply. |
| Errors | Typed with stable public codes (config, trust, policy, diagnosis). User-facing text never leaks stack traces or secrets. |
| Observability | `slog` JSON with `tenant, repo, pr, env_id, deployment_id, generation`; OpenTelemetry context from webhook → queue → orchestrator → agent. |
| Docs | Each phase updates `docs/`; costly decisions get an ADR. |
| Windows dev | Docker Desktop (WSL2) for kind; every `make` target maps to a plain `go` command. |

---

## P0 - Scaffold, config schema, `heimdall validate` (DONE)

Module layout, CI, strict loader, stable diagnostic codes, source-located errors,
"did you mean" hints, defaults, JSON output, ShopFlow example, ADRs 1-3.

## P0.1 - Configuration trust policy (DONE)

Implements ADR 0006's first slice.

- `config.Policy` (embeds `Limits`) replaces the bare limits argument to `Load`:
  `MaxVisibility`, `AllowedSecrets`, `AllowedRegistries` → `policy.*` errors.
- `config.CompareToBaseline(baseline, pr)` → `trust.visibility`, `trust.ttl`,
  `trust.secret`, `trust.resources`; positioned with the PR file's line numbers.
- `heimdall validate --baseline <default-branch file> <pr file>`.
- Unit tests for every denial and for allowed application-level changes.

**Still to do in this track:** tenant policy persistence and delivery (P5).
(`requests`, size presets and the JSON Schema export were delivered in P1.)

---

## P1 - Rendering and secure defaults (DONE)

**Delivered** (reference: [rendering.md](rendering.md)):

- `internal/render`: pure `Render(Config, Context) -> Plan`; five stages of
  ordered **steps** (`baseline-db` = prepare → migrate → seed → clone;
  `application` = `dependsOn` waves). Stable `render.*` error codes, all
  problems reported in one pass.
- Secure defaults in every pod from one builder; namespace enforces Pod
  Security `restricted`; default-deny NetworkPolicy with DNS, gateway and
  optional egress allows (metadata ranges always excluded); quota forbids
  LoadBalancer/NodePort services.
- `resources.size` presets and `resources.requests` with the revised defaults;
  memory over-commit capped by policy (`resources.requests.too_low`) and by
  the LimitRange; `large` gated by `Policy.AllowLargeSize`.
- Naming, labels, injected env, digest-only images, a pinned catalog for
  dependency images; `heimdall render` (`--list`, `--stage`, `--out-dir`);
  `heimdall schema` and the committed `schema/heimdall.schema.json`.
- `charts/heimdall-agent` with two-tier RBAC and ValidatingAdmissionPolicies,
  documented in its README.
- The runnable ShopFlow demo app, and `test/e2e/kind` (the exit criterion).

**Exit criteria evidence:** `make e2e-kind` renders ShopFlow, applies it step
by step to kind and reaches Ready: seed data served through a real Gateway, an
order flowing API → RabbitMQ → worker → PostgreSQL, live security probes (no
token, non-root, read-only root, Pod Security, quota, ingress and egress
isolation with positive controls), the agent's permissions checked by
impersonation, re-apply a no-op, render deterministic. Security-defaults,
golden, property (400 random configs) and fuzz tests pass; every rendered
object and the chart pass `kubeconform -strict`. Baseline timings with warm
images: about 100 s from empty namespace to smoke tests passed (dependencies
51 s, RabbitMQ boot dominating).

**Decisions made while implementing** (beyond the plan below):

| Decision | Why |
|---|---|
| Gateway API `HTTPRoute`s on one shared Gateway, not `Ingress` ([ADR 0008](adr/0008-gateway-api-for-preview-routing.md)) | Keeps the wildcard certificate out of preview namespaces; ingress-nginx is retired |
| Credentials are a `Context` input; render emits the Secret only when given them | Keeps render pure; the engine generates once and passes them back |
| Migrations and seed run as the unprivileged `app` role; only Heimdall's scripts use the superuser | Seed SQL is PR-controlled (no `COPY ... PROGRAM`) |
| Baseline rebuilt every generation; seed in an immutable per-generation ConfigMap (≤ 768 KiB) | Preview reflects exactly its commit; bigger fixtures move to the P3 bundle |
| Generation on object metadata only, not on long-running pod templates | A push must not restart Postgres; extending the TTL restarts nothing |
| Deployments roll with `maxSurge: 0` | Rollouts never need more than the quota |
| Quota = tenant ceilings + largest Job step | Ceilings stay authoritative; Jobs have room |
| Dependency images from ECR Public / quay.io, pinned by index digest, mirrorable | Docker Hub anonymous limits break clusters (and did break this work's first e2e attempt) |
| Workload names may not start with `heimdall-`; injected env names reserved (`env.reserved`) | No collisions with platform objects or injected variables |
| App containers run as UID 10001 | Images defaulting to root (node, python) still run, non-root |
| RabbitMQ: one Erlang scheduler, no busy-wait, no plugins | Boot at 250m CPU: 83 s → 42 s (measured) |

**Carried forward:** P2 prunes by generation and recreates the Postgres
StatefulSet when its storage changes (immutable claim templates); P3 builds
the agent the chart installs, and the agent must verify its admission policy
is enforced before acting (policies load asynchronously; the e2e observed the
gap right after install); RabbitMQ 4.x and Postgres 18 can join the catalog
with an e2e run each.

### Original plan

**Goal:** turn a validated `Config` into typed Kubernetes objects,
deterministically, per pipeline stage, with no cluster access.

**Scope**
- `internal/render`: pure function `(Config, RenderContext{tenant, repo, pr, sha,
  generation, imageDigests, baseDomain}) -> []Stage`, each stage a list of typed
  objects (client-go types). Stages are exactly the pipeline in ADR 0007:
  `guardrails → dependencies → baseline-db (migrate, seed) → application → smoke`.
- `heimdall render` prints a stage's objects as YAML for inspection and golden tests.
- **Secure defaults, rendered into every pod:** `runAsNonRoot`, read-only root
  filesystem where possible, `seccompProfile: RuntimeDefault`, drop all
  capabilities, no privilege escalation, **no service-account token**, no AWS
  credentials. Default-deny `NetworkPolicy` plus explicit allows (intra-namespace,
  DNS, gateway ingress, egress allowlist).
- **Resources (revised):** `resources.cpu/memory` remain the limits; new optional
  `resources.requests`. Defaults: CPU request 20% of limit (burst), **memory
  request 50% of limit, floor 64Mi**; platform caps node memory over-commit.
  **Size presets** `small | medium | large` (`large` requires admin approval via
  policy). A stack of small/medium/large is a golden path for teams.
- **Naming and URLs (revised):** one wildcard certificate per cluster, not per
  tenant. Hostnames are readable plus a short random suffix:
  `pr184-shopflow-x7d2.<preview-domain>` (primary) and
  `pr184-shopflow-api-x7d2.<preview-domain>` (other exposed services). Tier 1:
  the customer's domain and cert; Tier 0: a single `*.preview.heimdall.dev`.
- **Injected env:** `PORT`, `DATABASE_URL`, `REDIS_URL`, `AMQP_URL`,
  `HEIMDALL_PUBLIC_URL`, `HEIMDALL_PUBLIC_URL_<SERVICE>`, `HEIMDALL_PR`,
  `HEIMDALL_SHA`. Frontends read URLs at **runtime**.
- **Images by digest** only; objects labelled
  `heimdall.dev/{tenant,repo,pr,env,generation,owner,expires}`.
- `charts/heimdall-agent`: Helm chart to **install the agent** (the only Helm in
  the product's data path). Minimal RBAC, documented.
- JSON Schema generated from the Go types (`heimdall schema`) for editor
  autocompletion.

**Tests:** golden objects for ShopFlow + minimal + each optional feature;
`kubeconform` in CI; determinism property test (same input → byte-identical);
a test asserting **no pod** is ever rendered with privileged settings, host
mounts, a mounted token, or `latest`/tag-only images.

**Exit criteria:** applying `heimdall render` output (stage by stage) to kind
brings ShopFlow to Ready; the security-defaults test passes.

**Risks:** typed builders are verbose (mitigate: small helper layer, golden
tests); supported dependency versions must be pinned and agree with
`defaults.go`.

---

## P2 - Local engine and data lifecycle (`up`, `down`, `reset`, `logs`, `status`)

**Goal:** an idempotent library that drives a namespace to a `Spec`, wrapped in a
CLI. The controller in P3 is this engine plus Kubernetes plumbing.

**Scope**
- `internal/engine`:
  - `Apply(ctx, Spec)` runs the stages from P1 with **server-side apply** (single
    field manager), waits for each stage's readiness via watches with deadlines,
    and **prunes** objects of older generations after success.
  - Input is an explicit `Spec` (including `generation`). Labels/annotations are
    informational; the engine never reads them to decide intent (ADR 0005).
  - `Destroy`: delete in order (workloads → PVCs → namespace), verify gone.
- **Baseline and live database (revised):**
  ```text
  start PostgreSQL -> create baseline DB -> run migration Job
  -> load synthetic seed Job -> freeze baseline (template, connections denied)
  -> clone live DB from baseline
  ```
- **Reset (revised to avoid races):**
  ```text
  suspend API + workers (scale to 0)
  -> terminate remaining connections -> drop live DB
  -> recreate live DB from baseline template
  -> flush Redis, purge/re-declare RabbitMQ topology
  -> resume API + workers -> wait ready -> run smoke check
  ```
  Reset is a single audited operation with its own stage events.
- **No synthetic-data shortcuts:** MVP uses only synthetic seed data or manually
  approved sanitised data; the seed loader rejects production-looking connection
  strings/hosts and refuses to run outside a Heimdall namespace.
- **Force cleanup is break-glass (revised):** normal `Destroy` never force-deletes.
  A stuck namespace surfaces as a failed `destroying` state with a diagnosis. An
  administrator-only `force-cleanup` action exists, requires a reason, is written
  to the audit log, and preserves evidence (events and object dumps) first.
- CLI refuses kube-contexts not on an allowlist (prevents running `down` against
  a real cluster by accident).

**Tests:** engine against fake clients; `envtest` for apply/prune/delete; kind
e2e (permanent CI): *two PRs side by side, mutate data in one, other unaffected,
reset one while its API is under load without errors, destroy one, other survives.*

**Exit criteria:** the demo from the brief runs from the CLI on kind (port-forward
or `*.localtest.me` for URLs). Stage timings are recorded as a baseline for the
time-to-ready SLO.

---

## P3 - Agent, controller and `PreviewEnvironment` CRD

**Goal:** declarative, self-healing, **outbound-only** environments with
guaranteed cleanup (ADR 0004/0005).

**Scope**
- `cmd/agent` = controller + sync loop in one binary, installed by
  `charts/heimdall-agent`.
- CRD `PreviewEnvironment` (`heimdall.dev/v1alpha1`):
  - `spec`: tenant, repo, PR, sha, **generation**, config bundle digest, image
    digests, expiry, `desiredState: running | destroyed`, `resetNonce`.
    (`sleeping` is **not** accepted until P8; the API returns `not supported`.)
  - `status`: phase, per-stage conditions, URLs, `observedGeneration`, last error
    (code + message).
- **Pluggable `DesiredStateSource`:** in P3 it is a local file / `kubectl apply`;
  in P5 it becomes the control-plane API (outbound pull/stream). The phase is
  demoable with no control plane.
- Reconciler: engine stages behind conditions; level-triggered, idempotent,
  exponential backoff; finalizer runs `Destroy`, then releases the object.
- **Generation fencing at the controller:** all Jobs/objects carry the
  generation label; a result from an older generation is discarded; starting
  generation N+1 cancels N's stages.
- **Sweeper (revised, fail-safe):** deletes an orphan only if ownership labels
  verify **and** it is older than a grace period **and** a second reconcile pass
  agrees **and** desired state was obtained authoritatively. If the source is
  unreachable it does nothing. Dry-run mode emits events only.
- Leader election, probes, Prometheus metrics.
- Admission webhook reusing `internal/config` with the tenant `Policy`; rejects
  specs not signed/received through the agent path.

**Tests:** `envtest` reconcile transitions (create → ready, spec change →
rollout, delete → finalizer cleanup, crash mid-stage → resume); a **stale
generation** test (slow gen-1 Job finishing after gen-2 started changes nothing);
sweeper tests including "source unreachable → no deletions"; chaos: kill the
agent in each stage.

**Exit criteria:** `kubectl apply` a `PreviewEnvironment` → ready; delete →
fully gone; kill the agent mid-way → converges; hand-labelled orphan removed only
after the grace period and not at all when the source is down.

---

## P4 - Diagnostics engine ("what failed and what do I do")

*Can be developed in parallel with P3: it is a pure function over recorded
snapshots.*

**Scope**
- `internal/diagnose`: `Snapshot -> []Diagnosis`; Snapshot = pods/containers,
  events, Job statuses and log tails, ingress endpoints, quota usage.
- Stable public codes: `IMAGE_PULL_FAILED`, `CONTAINER_CRASH`, `OUT_OF_MEMORY`,
  `QUOTA_EXCEEDED`, `NO_CAPACITY`, `HEALTHCHECK_FAILED`, `NO_ENDPOINTS`,
  `DB_UNREACHABLE`, `MIGRATION_FAILED`, `SEED_FAILED`, `SMOKE_TEST_FAILED`,
  `CONFIG_INVALID`, `POLICY_DENIED`, `STALE_GENERATION`.
- **Migration error parsing** (the ShopFlow card): Postgres SQLSTATE → `23502`
  "contains null values" → non-null column without default (table, column,
  suggestion); `42701` duplicate column; `42P07` relation exists; `42601` syntax.
- Root-cause ranking: a failed migration outranks "api not ready".
- **Log hygiene (revised):** tail size-capped; redact *known* secret values
  (the agent knows the exact values it injected) plus pattern scanning; heuristics
  are a safety net, **not** the primary control. Previews carry no powerful cloud
  secrets by default (P7). Full logs stay in the customer's account and are
  fetched on demand through the agent, authorised per user (ADR 0004). The control
  plane stores only codes, summaries and short redacted evidence.
- Renderers: PR comment markdown, CLI, JSON.
- LLM summarisation: optional, later, on top of rule output, redaction first.

**Tests:** golden fixtures per rule from real captured pod/event JSON; noise-
immunity test (unrelated events don't change the ranked root cause); exact-text
snapshots for the PR comment; redaction tests with planted secrets.

**Exit criteria:** break each scenario on kind (bad image, bad migration, OOM,
quota, crashing app, failing smoke test); each yields the expected code and an
actionable message.

---

## P5 - Control plane: state, API, audit, generation fencing

**Scope**
- goose migrations + sqlc queries: tenants, installations, repositories,
  clusters, environments, **deployments (with `generation`)**, events,
  diagnoses, smoke_runs, quotas, usage_samples, audit_log, `webhook_deliveries`
  (unique `delivery_id` = the durable inbox), outbox, tenant policy.
- `internal/domain`: one guarded `Transition()`, compare-and-swap on environment
  `version`, an `events` row in the same transaction. **Status updates with a
  stale generation are rejected and recorded as `stale` events** (ADR 0005).
- OpenAPI-first API with generated server/client; SSE timeline stream; idempotency
  keys; keyset pagination.
- **Agent endpoints (outbound-only):** register, heartbeat, pull desired state,
  push status/log-on-demand. Agent identity: per-cluster credential (short-lived
  tokens, rotatable); the API never calls into the cluster.
- **Tenancy (revised):** application DB role **without `BYPASSRLS`** and not the
  table owner; `FORCE ROW LEVEL SECURITY`; tenant set inside each transaction with
  `set_config('app.tenant_id', $1, true)` (`SET LOCAL`) from the authenticated
  principal, never from a request field. Tests prove cross-tenant reads fail,
  including via a missing-context query.
- Audit log for every mutation; outbox for GitHub/Slack notifications.
- Tenant **policy** storage and delivery to `config.Load` / the admission path.

**Tests:** testcontainers Postgres; state-machine from→to table; RLS adversarial
tests; stale-generation tests; OpenAPI contract tests.

**Exit criteria:** via the API only, create / retry / reset / delete an
environment on a kind cluster running the agent; timeline shows each stage.

---

## P6 - GitHub integration and the secure CI build contract

**Scope**
- GitHub App (permissions: pull_requests, checks, metadata, **contents: read**
  for `heimdall.yaml`); installation tokens minted per use.
- Webhook Lambda: HMAC verify (constant time), size cap, enqueue to SQS FIFO
  (`MessageGroupId = repo#pr`), DLQ + alarm. **Durable dedupe happens in
  PostgreSQL**, not in SQS's 5-minute window.
- Orchestrator: open/sync → new generation; close/merge → destroy. A new push
  supersedes in-flight work (fenced by generation).
- **Slash commands authorised (revised):** `/heimdall reset|retry|extend|delete`
  are honoured only if the commenter has write/maintain permission on the repo,
  verified by the GitHub API (not by trusting payload fields), and every command
  is audited. Others get a polite denial.
- **Fork PRs (revised): disabled in the MVP.** A label is not enough protection
  for untrusted code. A later design needs a separate restricted build path with
  no cloud credentials.
- **Secure CI contract:** the reusable workflow builds images and the config/
  fixture **bundle** in the customer's Actions using OIDC to the customer's
  registry (no stored keys), scans (Trivy), pushes by digest, then notifies the
  API with digests only. Pull-request workflows never get production credentials.
  Trust check (ADR 0006) runs on receipt: PR config vs default-branch config vs
  tenant policy.
- PR comment created once and **edited**; Checks API status so branch protection
  can require "Heimdall Preview". `heimdall init` prints the files for the user to
  commit (no write permission by default; an opt-in write scope can be added with
  an explanation).
- Rate-limit handling: ETags, retry with jitter, outbox.

**Tests:** recorded webhook fixtures; fake GitHub server; LocalStack SQS FIFO
ordering; duplicate/out-of-order replay; authorisation tests for slash commands.

**Exit criteria:** on a real test repo, PR #101 and #102 create two separate
environments; pushing to #101 updates it; closing #101 removes only it; a
non-collaborator's `/heimdall delete` is refused.

---

## P7 - Private gateway, isolation, secrets, supply chain

**No preview URL is exposed beyond a trusted team before this phase passes.**

**Scope**
- **Preview gateway:** OIDC auth in front of ingress (oauth2-proxy or ALB OIDC),
  GitHub org/team checks, `private | org | public` from config (bounded by
  policy), signed expiring share links.
- **Isolation (Tier-aware):** default-deny NetworkPolicy verified by tests; Pod
  Security `restricted`; a **dedicated, tainted preview node pool**; no host
  mounts/privileged pods; IMDSv2 with hop limit 1; seccomp default. Optional
  gVisor/Kata RuntimeClass.
- **No AWS credentials in previews by default (revised).** No per-PR IAM roles
  (they don't scale and widen blast radius). Fixture and S3 access goes through
  the agent (it fetches the bundle) or short-lived presigned URLs / a narrow
  broker; any exception is explicit, audited, policy-gated.
- **Secrets:** per-repo preview secrets live in Secrets Manager (customer's
  account in Tier 1), delivered by External Secrets, never in CRs, RDS, logs or
  config; only names listed in `AllowedSecrets`; generated per-environment
  credentials (DB passwords).
- **Supply chain:** vulnerability scan gate, base-image policy, dependency
  images by digest, SBOM stored with the deployment record.
- **Abuse controls:** CPU/egress limits against miners, per-tenant concurrency
  caps, alerting on sustained saturation.
- STRIDE threat model per component.

**Tests:** adversarial e2e on kind (cross-namespace traffic, Kubernetes API,
control plane, metadata endpoint, disallowed egress, privilege-escalation
attempts) - plus the **EKS security suite in P9/P10**, since kind cannot prove
IMDS or VPC-CNI behaviour.

**Exit criteria:** adversarial suite green on kind; unauthenticated request to a
private URL redirects to login; non-members are denied.

---

## P8 - Lifecycle, quotas and cost

**Scope**
- TTL enforcement, expiry warnings (T-4h) on the PR, `/heimdall extend`
  (authorised, bounded by policy).
- **Manual sleep/wake first (revised):** `sleep` scales workloads to zero and keeps
  PVCs; `wake` restores them. **Automatic wake-on-request (KEDA HTTP add-on or a
  gateway holding requests) moves to a later phase** after the core flow is
  reliable.
- Quotas at namespace (ResourceQuota), tenant (environments, CPU, memory, DB GB;
  enforced at API and admission) and cluster level (capacity-aware, queue rather
  than fail).
- **Metering as estimated allocation cost (revised):** the report is labelled
  "estimated". Model = resource requests × time, plus storage, plus an apportioned
  share of fixed cluster costs (control plane, NAT, LB) and node cost; it does not
  claim to be an invoice. Grouped by tenant / repo / PR.
- Karpenter with spot for the preview pool, consolidation on, node TTL; log
  retention via S3 lifecycle rules.

**Tests:** fake clock for TTL/sleep; quota admission tests; metering accuracy
against synthetic workloads.

**Exit criteria:** an environment sleeps and wakes on command, expires on
schedule with a warning, and the usage report matches the model within tolerance.

---

## P9 - AWS infrastructure and EKS validation

**Scope (Terraform modules)**
- VPC, private subnets, endpoints (avoid NAT cost where possible), EKS with
  system + preview (spot) node groups, Karpenter, VPC CNI network policy, EBS CSI.
- ECR, S3, RDS PostgreSQL (control plane, Tier 0), SQS FIFO + DLQ, API Gateway +
  Lambda, Secrets Manager, Route 53 + ACM, WAF.
- Add-ons by Helm (cert-manager, ExternalDNS, ingress, External Secrets,
  monitoring); **Argo CD only if platform drift warrants it** (ADR 0007).
- GitHub Actions OIDC trust; deploy pipeline for Heimdall itself.
- **`make cloud-up` / `make cloud-down`** create and destroy the whole demo
  environment; a script verifies nothing billable is left; budgets and alarms.
- **EKS security suite:** IMDS blocked from pods, network policies enforced by the
  VPC CNI, pod identity isolation, cross-namespace denial, node-pool placement.

**Exit criteria:** from an empty account, `cloud-up` + one deploy command yields
a working system; the P6 demo passes; the EKS security suite passes; `cloud-down`
leaves no orphans.

---

## P10 - Reliability, observability, real-EKS security

**Scope**
- OpenTelemetry end to end; RED metrics; dashboards, alerts with runbooks.
- SLOs: time-to-ready p95, cleanup after close (100% within N minutes),
  webhook-to-first-comment p95; burn-rate alerts.
- Load/soak: 50 concurrent environments, push storms, GitHub outage and
  rate-limit simulation.
- Chaos: kill agent/orchestrator/API mid-deploy, drop SQS, RDS failover; the
  system converges without manual action and stale generations stay harmless.
- Backup/restore and DR for RDS; expand/contract migrations; CRD upgrade path.
- Security scans in CI (govulncheck, Trivy, gosec); re-run the EKS security suite
  on a schedule.

**Exit criteria:** SLOs measured; chaos suite green; a runbook per alert.

---

## P11 - Dashboard, onboarding and launch demo

**Scope**
- Minimal React dashboard (environments, timeline, per-service logs via agent,
  diagnosis card, retry/reset/extend/delete, estimated cost).
- Onboarding: install GitHub App → install agent (Helm) → `heimdall init` prints
  `heimdall.yaml` and the workflow for the user to commit.
- Docs site; troubleshooting by diagnosis code.
- Scripted demo with a recorded backup: ShopFlow PR #184/#185, parallel
  environments, data isolation, failed-migration card, reset, close/cleanup, and a
  PR that tries to loosen visibility and is refused by the trust check.

**Exit criteria:** a new user reaches a working preview in under 30 minutes using
only the docs.

---

## P12 - After launch

Automatic wake-on-request, fork-PR previews (restricted build path), Kafka and
object-storage mocks, WireMock service mocks, masked production snapshots (legal
review), GitLab, Slack app, SSO/SAML, billing, multi-region, Tier 2 managed
per-tenant clusters, LLM-assisted diagnosis.

---

## Risk register

| Risk | Impact | Mitigation |
|---|---|---|
| Hostile code sharing a kernel with other tenants | Critical | Tiers (ADR 0004): Tier 0 trusted only; Tier 1 customer clusters; dedicated tainted node pool; adversarial suites on kind **and** EKS |
| Wrong/stale state after a crash or race | High | One owner per fact; generation fencing; durable inbox in Postgres; CAS transitions |
| Orphans / runaway cost | High | Finalizers, fail-safe sweeper, quotas, budgets, `cloud-down` verification |
| Config used as an escalation path | High | Policy + baseline comparison (ADR 0006); dangerous knobs not in schema |
| Over-packed nodes / OOM kills | Medium | Memory requests near working set; size presets; node over-commit cap |
| GitHub limits/outages | Medium | Outbox, jittered retries, ETags; environments keep working |
| Cold start too slow | Medium | Pre-pulled images, template DB, measured stage timings from P2 |
| Scope creep | High | P12 gate; exit criteria per phase; ADRs for deviations |
| AWS spend above budget | Medium | Three operating modes; demo clusters short-lived; honest cost labelling |

---

## Revision 2: what changed and why

| # | Change | Phase(s) | Reason |
|---|---|---|---|
| 1 | Isolation tiers; Tier 1 customer-owned cluster with an outbound-only agent is the product model; shared multi-tenant cluster unsupported | P3, P5, P7 | Namespaces are not a hard boundary for hostile multi-tenant code (ADR 0004) |
| 2 | One owner per fact; labels are metadata only; agent creates the CR from pulled desired state | P2, P3, P5 | Three sources of truth diverge after failures (ADR 0005) |
| 3 | Deployment `generation` fencing end to end | P3, P5, P6 | Old deploy must not mark a newer one ready |
| 4 | Durable dedupe in Postgres, not SQS FIFO | P5, P6 | FIFO dedupe window is only 5 minutes |
| 5 | Controller applies typed objects with SSA; Helm only ships Heimdall; no hooks; Argo CD optional | P1, P2, P9 | Two tools owning one object conflict; hooks are hard to recover (ADR 0007) |
| 6 | Migration/seed/smoke as explicit stages and Jobs | P1, P2 | Visible, retryable, diagnosable |
| 7 | Config trust: policy ceilings + default-branch baseline; `policy.*`/`trust.*` codes | P0.1, P5, P6 | PR can edit its own limits (ADR 0006) - implemented now |
| 8 | Memory requests near working set; size presets; configurable requests | P1 | 25%-of-limit rule causes over-packing and OOM |
| 9 | One wildcard cert per cluster; readable + random-suffix hostnames | P1 | Per-tenant certs are noisy and hit issuance rate limits |
| 10 | Database baseline → clone; reset suspends workloads first | P2 | Reset raced with connected pods |
| 11 | Force-cleanup is audited break-glass only | P2 | Silent force-delete leaks resources and loses evidence |
| 12 | Sweeper: grace + second pass + authoritative source, else no-op; dry-run | P3 | Temporary outages must not delete live environments |
| 13 | `sleeping` rejected until P8; manual sleep first, auto-wake later | P3, P8 | Don't ship a state we can't honour; KEDA is heavy |
| 14 | Redaction by known values first, patterns second; logs stay in customer account | P4, P7 | Heuristics are unreliable; data minimisation |
| 15 | Application DB role without BYPASSRLS, `FORCE RLS`, `SET LOCAL` tenant | P5 | RLS is easy to bypass accidentally |
| 16 | Fork PRs disabled in MVP; slash commands authorised via repo permission | P6 | A label or a comment is not authorisation |
| 17 | No per-PR IAM roles; previews get no AWS credentials by default | P7 | Doesn't scale; widens blast radius |
| 18 | EKS security suite in P9/P10 | P9, P10 | Kind cannot prove IMDS/VPC-CNI behaviour |
| 19 | Cost reported as *estimated allocation cost* | P8 | CPU/memory metrics alone mislead |
| 20 | Three operating modes; permanent EKS stack not planned | all | Budget realism |
| 21 | `heimdall init` prints files; no repo-write permission by default | P6, P11 | Least privilege for the GitHub App |
| 22 | P7 gates exposing preview URLs | P7 | Public URLs before auth is unsafe |

**Considered and kept as-is:** SQS FIFO for per-PR ordering (still right for
ordering); Postgres for control-plane state; Go-native typed rendering (chosen
over templates for compile-time safety and first-class stages).
