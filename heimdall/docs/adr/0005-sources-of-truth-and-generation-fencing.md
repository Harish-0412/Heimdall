# ADR 0005: Sources of truth and generation fencing

Status: accepted (amends ADR 0002)

## Context
The first plan let three places describe an environment: namespace
labels/annotations (P2), the `PreviewEnvironment` CR (P3), and the PostgreSQL
control plane (P5). After a crash they can disagree, and a slow deployment of an
old commit can finish after a newer one and overwrite its result.

## Decision

### Each fact has exactly one owner

| Layer | Owns | Never owns |
|---|---|---|
| **PostgreSQL (control plane)** | *Desired* environment per PR: revision, desired state, tenant, owner, expiry, audit history, GitHub delivery state, billing | Runtime facts |
| **`PreviewEnvironment` CR** | Desired state of one environment **for one cluster**, a projection of the database row | Anything not derivable from the database |
| **CR `status` + Kubernetes objects** | *Actual* runtime state | Intent |
| **Namespace labels** | Searchable metadata only (tenant, PR, owner, expiry, deployment id) | Authority. Never read to decide what should exist |

The CR is created and updated by the **agent**, not by the control plane (the
control plane has no network path to the cluster, ADR 0004):

```text
GitHub PR event
  -> webhook Lambda (verify HMAC, enqueue)         [SQS FIFO: ordering, not dedupe]
  -> orchestrator inserts into webhook_deliveries  [UNIQUE(delivery_id): durable inbox]
  -> control plane records a new desired revision (deployment generation N)
  -> agent syncs desired state (outbound pull/stream), applies the CR
  -> controller reconciles the namespace, writes CR status (observedGeneration)
  -> agent reports status -> control plane accepts only if generation is current
  -> outbox -> GitHub PR comment / check
```

SQS FIFO deduplication only covers a 5-minute window, so the durable dedupe is
the unique constraint in PostgreSQL, not the queue.

### Generation fencing
Every push or action creates a new `deployment` with a monotonically increasing
`generation` per environment (a database sequence/`max+1` under row lock).

- The generation is written to `spec.generation` in the CR and as a label on every
  object and Job the controller creates.
- The controller records `status.observedGeneration` and reports it.
- The API **rejects status updates whose generation is not the current one** and
  records them as `stale` events. An old Job can therefore never mark the
  environment ready, never edit the PR comment, and never mark a newer
  deployment failed.
- Starting generation N+1 cancels in-flight work for N (Jobs deleted, stage
  contexts cancelled), but correctness does not depend on the cancel arriving.
- Compare-and-swap on environment `version` for every state transition.

### Sweeper safety
The sweeper treats the control plane's desired state as truth. It only deletes an
orphan when all hold: ownership labels verify, the object is older than a grace
period, a **second** reconcile pass agrees, and the control plane was reachable
and answered authoritatively. If the control plane cannot be reached the sweeper
**does nothing** (fail safe). It supports a dry-run mode that only emits events.

## Consequences
- P2's "labels are the record of what was deployed" is withdrawn; labels are
  informational. The engine's input is an explicit `Spec`.
- P3 runs without the control plane: the agent's `DesiredStateSource` is
  pluggable (a local file or `kubectl apply` in P3; the API in P5).
- More plumbing (generation, outbox, stale-event handling) but failures become
  well-defined instead of racy.
