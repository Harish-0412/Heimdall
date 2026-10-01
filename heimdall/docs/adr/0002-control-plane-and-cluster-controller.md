# ADR 0002: Control plane decides, in-cluster controller converges

Status: accepted - **amended** by [ADR 0004](0004-tenant-isolation-and-cluster-ownership.md)
(outbound-only agent; CR created by the agent) and
[ADR 0005](0005-sources-of-truth-and-generation-fencing.md) (single owner per fact;
generation fencing). Where this text says "the control plane creates the CR",
read "the agent creates it from desired state it pulled".

## Context
Preview environments are long-running, failure-prone and must be cleaned up
reliably. Provisioning needs cluster credentials and can exceed Lambda limits.
We also want a path to customer-owned clusters.

## Decision
- **Control plane** (API + orchestrator, backed by PostgreSQL/SQS) owns product
  state: tenants, runs, events, quotas, audit.
- A `PreviewEnvironment` custom resource carries the *desired* cluster state.
- A **controller** running in each target cluster reconciles that resource using
  a shared `internal/engine` library, and reports status back through the API.
- The controller never writes to the control-plane database.
- The CLI uses the same engine, so `heimdall up` locally exercises production code.

## Consequences
- Reconciliation is idempotent and level-triggered; retries and crash recovery
  come from controller-runtime instead of hand-rolled workflow code.
- Customer clusters only need to run the controller and reach the API outbound.
- Two sources of truth exist (RDS and the CR). Drift is handled by a sweeper that
  reconciles namespaces, rows and GitHub PR state on a schedule.
- More moving parts than "a worker that runs helm"; accepted for the cleanup and
  multi-cluster guarantees.
