# Backend design

Condensed from the design discussion. Decisions with long-term cost live in
[ADRs](adr); per-phase detail lives in [phases.md](phases.md).

## Components

| Component | Runs on | Responsibility |
|---|---|---|
| `heimdall-webhook` | Lambda + API Gateway | Verify GitHub HMAC, dedupe by delivery id, enqueue. Nothing else. |
| `heimdall-api` | Container (control namespace) | REST/SSE API, owns PostgreSQL, tenant scoping, audit. Stateless. |
| `heimdall-orchestrator` | Container | Consumes SQS FIFO; decides deploy/destroy/retry; updates PR comment & checks. |
| `heimdall-agent` | Every target cluster (customer's own in the product model, ADR 0004) | Outbound-only. Pulls desired state from the API, creates `PreviewEnvironment` CRs, runs the controller + `internal/engine`, sweeper, pushes status. |
| `heimdall` CLI | Dev machine / CI | `validate`, `render`, `up`, `down`, `reset`, `logs`, `status`. Same engine as the agent. |

```text
GitHub -> API GW -> webhook λ -> SQS FIFO (group repo#pr: ordering)
       -> orchestrator -> webhook_deliveries (UNIQUE: durable dedupe) -> new generation N in RDS
                                                                   ▲ status (accepted only if generation is current)
 agent (in cluster) ── pulls desired state (outbound) ───────────┐ │
   creates PreviewEnvironment CR -> controller + engine -> namespace pr-N
   pushes CR status ─────────────────────────────────────────────┴─┘
```

Who owns what: PostgreSQL = desired state and history; CR = desired state for one
cluster; CR status + cluster objects = actual state; namespace labels = metadata
only (ADR 0005). Trust: tenant policy ∩ default-branch config ∩ PR config; a PR can
only narrow (ADR 0006).

## Stack

Go; chi + OpenAPI (oapi-codegen); PostgreSQL via pgx + sqlc + goose; SQS FIFO + DLQ;
controller-runtime with typed client-go objects and server-side apply (Helm only
ships Heimdall itself, ADR 0007); Gateway API for preview routing (ADR 0008);
go-github (GitHub App); OpenTelemetry + Prometheus + `slog`; Terraform; kind +
LocalStack for local development.

## Environment state machine

```text
pending -> building -> provisioning -> migrating -> seeding -> testing -> ready
   any stage -> failed   (retry | reset | delete valid from failed)
ready -> resetting -> ready;  ready -> sleeping -> waking -> ready
any -> destroying -> destroyed
```

One guarded `Transition()` function; every transition writes an `events` row in the
same transaction. A new push supersedes the in-flight *deployment*; the
*environment* persists.

## Data model (control plane)

`tenants`, `installations`, `repositories`, `clusters`, `environments`
(unique on repo+PR while not destroyed; images by digest), `deployments`
(store the config snapshot so retry replays exactly what ran), `events`
(timeline), `diagnoses`, `smoke_runs`, `quotas`, `usage_samples`, `audit_log`,
`webhook_deliveries`. Row-level security on tenant id; outbox for notifications.

## ShopFlow walk-through

1. PR #184 opened → webhook → queue → orchestrator creates `environments` row.
2. CI (customer's Actions, OIDC) builds `web`, `api` → pushes digests to ECR → calls API.
3. The agent pulls the new desired state and writes a `PreviewEnvironment` CR
   (namespace `heimdall-pr184-shopflow-x7d2`).
4. Controller applies the rendered stages ([rendering.md](rendering.md)):
   guardrails → dependencies (Postgres, Redis, RabbitMQ) → baseline database
   (prepare, migration Job, seed Job, freeze + clone `app` from
   `app_baseline`) → application in `dependsOn` waves → smoke-test Jobs → ready.
5. URLs `https://pr184-shopflow-x7d2.<preview-domain>` (web, primary) and
   `https://pr184-shopflow-api-x7d2.<preview-domain>`, routed by `HTTPRoute`s
   on the cluster's shared Gateway (ADR 0008); PR comment edited in place.
6. On failure, the diagnosis engine turns the migration Job's SQLSTATE into the
   "non-null column without default on `orders`" card.
7. PR closed → CR deleted → finalizer destroys workloads, PVCs, S3 prefix,
   namespace; sweeper verifies. PR #185 is untouched.

## Principles

- Treat every PR as hostile input. Strict config, size caps, path containment.
- Idempotent, level-triggered reconciliation; no hidden state outside labels/CRs/DB.
- One config loader and one engine shared by CLI, API and controller.
- Stable machine codes (config diagnostics, failure diagnoses) as public contracts.
