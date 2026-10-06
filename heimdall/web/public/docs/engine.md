# Local engine (Phase 2)

`internal/engine` drives the P1 render plan through the Kubernetes API. The
CLI is a caller of the same library that the P3 controller can use. It does not
shell out to Helm or kubectl to reconcile workloads.

## Architecture and ownership

- `Spec` carries the loaded configuration, complete render context, explicit
  generation and optional operator data approval. Labels never reconstruct
  intent. A content digest rejects changed inputs under the same generation.
- `Cluster` isolates Kubernetes operations from reconciliation. Its production
  adapter uses one SSA field manager (`heimdall-engine`), UID/resourceVersion
  deletion preconditions and reconnecting list/watch readiness checks.
- `heimdall-operation` is a per-preview ConfigMap with a compare-and-swap
  operation lease, accepted generation/digest, reset nonce and the latest 128
  stage events. Concurrent CLI mutations fail with `engine.busy`. Renewal
  failure cancels work. A crashed process releases its lease after 60 seconds.
  The journal records execution; it is not a desired-state source.
- Every step has a configurable deadline and a start/completion/failure event
  with elapsed time. An `Observer` can export the full event stream. P5 supplies
  central, durable audit storage; the local bounded journal is not that service.
- Namespace and object ownership must match tenant, repository, PR, environment
  and managed-by labels. The CLI additionally pins its saved intent to the
  kube-context, API endpoint and cluster CA. Context names require an explicit
  exact allowlist entry, including for status and logs.

## Commands

Build with `go build -o bin/heimdall ./cmd/heimdall` (add `.exe` on Windows).
Build/push actual application images and provide their digest references via
`--images images.json` or repeated `--image name=reference` flags.

```bash
heimdall up examples/shopflow/heimdall.yaml \
  --context kind-heimdall --allow-context kind-heimdall \
  --state .heimdall/pr184.json --repo your-org/shopflow --pr 184 \
  --generation 1 --images images.json

heimdall status --state .heimdall/pr184.json --allow-context kind-heimdall
heimdall diagnose --state .heimdall/pr184.json --allow-context kind-heimdall
heimdall logs --state .heimdall/pr184.json --allow-context kind-heimdall --workload api
heimdall reset --state .heimdall/pr184.json --allow-context kind-heimdall --nonce 1
heimdall down --state .heimdall/pr184.json --allow-context kind-heimdall
```

Use a separate state file for each preview. `up` captures the exact configuration
and context before execution, allowing retries after a partial failure. The SHA
defaults to actual Git HEAD; outside a checkout supply `--sha`. Increase
`--generation` when changing configuration, images, commit or imported content.
The state file contains no generated credentials, but approved imported data can
be sensitive: `.heimdall/` is ignored by Git and files are created with mode 0600.
On Windows, protect the parent directory with the appropriate account ACLs.

Platform settings mirror the agent's `platform` configuration: `--base-domain`,
`--scheme`, `--url-port`, `--gateway`, `--storage-class`, `--egress-cidr`,
`--image-mirror` and `--node-selector key=value` (the preview node pool).

When `up` or `reset` fails, it prints the [diagnosis](diagnostics.md);
`--save-snapshot file` also keeps the redacted snapshot it was made from.
`--timeout` bounds each step. `--format json` emits a structured result; text mode
prints a readable summary and streams timings to stderr. Exit 0 means success,
1 means a failed operation
or rejected specification, 2 means invalid command usage. Status and bounded
logs use live API responses. Logs redact exact injected secret values before
pattern-based redaction; `--follow` requires one pod and an explicit container.

## Apply, pruning and storage

Apply follows `guardrails -> dependencies -> baseline-db -> application -> smoke`.
It waits for each ordered step. Readiness includes observed generation and all
desired replicas (Deployments: *available* replicas, so a worker must stay up
for its `minReadySeconds`), and Job failure terminates the wait immediately.
So does a pod failure that cannot recover without new input (a crash loop,
repeated out-of-memory kills, an image that cannot be pulled): the step fails
with `engine.workload_failed` instead of waiting out its timeout, and `up`
prints the [diagnosis](diagnostics.md). Completed per-generation Jobs are
reused on retry. Reapplying an unchanged, successful generation does not
quiesce or restart its workloads.

After all stages succeed, pruning removes older-generation objects absent from
the desired plan. It only considers owned resources in this namespace. Objects
with missing, malformed, equal or newer generation labels survive. Failed Jobs
survive a failed deployment. Platform-delivered application secrets and the
operation journal are excluded. Kubernetes garbage collection handles Pods,
ReplicaSets and StatefulSet claims through their owning workloads.

Postgres claim templates are immutable. A size or explicit storage-class change
quiesces application workloads, foreground-deletes the old StatefulSet, waits for
its old claim to disappear, then creates the desired StatefulSet and claim. The
baseline and live database are rebuilt for the new generation, using the existing
environment credentials. **Preview writes are discarded**, including for storage
growth. This is a rebuild, not an online volume expansion. An unrelated apply
rejection never triggers a destructive replacement. The implementation follows
the [StatefulSet storage lifecycle](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/).

## Reset and data provenance

No synthetic application data is supplied or generated. ShopFlow and the P2 e2e
use schema-only databases. If a config declares a seed file, `up` requires an
operator-owned JSON approval via `--seed-approval`. Its fields are `sha256`
(lowercase digest of the exact file), `approvedBy`, `reason`, and `sanitised: true`.
Keep that approval outside PR-controlled configuration. The library receives
the same attestation from its trusted caller.

Approval is a provenance assertion; software cannot infer whether arbitrary
records are real or sanitised. There is no synthetic-data mode, generated fallback
or production-source connector. The loader rejects external connection strings,
psql commands and unsupported SQL escape mechanisms. Paths are confined to the
repository, imports are bounded, and the Job runs as the unprivileged app role in
one transaction. Runtime guards enforce the expected preview namespace and the
in-namespace baseline target. NetworkPolicy supplies an additional boundary.

Reset has its own monotonic `--nonce`, independent of deployment generation:

1. Scale every existing API/service and worker to zero; wait for their Pods to
   terminate so connections and in-flight requests drain.
2. Force-disconnect remaining live-database sessions and clone the frozen
   baseline using a nonce-specific Job.
3. Recreate the emptyDir-backed Redis and RabbitMQ deployments. This clears all
   cache databases and queues. Applications must declare their RabbitMQ topology
   on startup, as ShopFlow does.
4. Resume the desired workload waves, wait ready and execute fresh smoke Jobs.

A completed nonce is a no-op. Retry an interrupted operation with the same nonce.
Failures during destructive work leave workloads suspended for safe recovery.
Reset necessarily has a maintenance window: requests issued while all API pods
are stopped may fail. Uninterrupted traffic would require request admission or
buffering outside the preview, which is not part of P2.

## Cleanup and break-glass

Normal `down` deletes Jobs and workloads, then Pods/PVCs, then the namespace,
and verifies deletion. It never removes finalizers or reduces grace periods.
Timeouts leave evidence and report `engine.destroy_stuck` or `engine.timeout`.

For an already terminating, owned namespace, an administrator can use:

```bash
heimdall force-cleanup --state .heimdall/pr184.json \
  --allow-context kind-heimdall --reason "Document the diagnosed failure" \
  --evidence cleanup-evidence.json
```

Kubernetes SelfSubjectAccessReview must grant namespace deletion and
`namespaces/finalize` update permission. The command discovers namespaced
resources and synchronously writes a new redacted evidence file before removing
owned-object finalizers. Foreign finalizers are refused. Namespace finalization
requires a fresh, complete inventory proving the namespace empty. A pending
controller cleanup reports a retryable failure, rather than claiming deletion.

## Verification

`go test ./...` runs the existing renderer/config suites and engine policy tests.
`make e2e-engine` (or `bash test/e2e/engine/run.sh`) builds actual ShopFlow images,
pushes them to a local registry and executes the CLI against kind. It validates
two previews, idempotent reapply, reset and nonce replay, actual Postgres storage
replacement, credential stability, generation pruning, removed workloads,
preservation of failed-deployment evidence and independent teardown. A separate
live test proves normal finalizer preservation and evidence-before-mutation cleanup.
Database mutation copies PostgreSQL's own live catalog; it creates no customer
records. API contract tests use the real Kubernetes API for concurrent journals,
generation fencing, lease recovery, competing field managers, selective pruning
and watch deadlines. They are explicitly
skipped when the dedicated kind context is not supplied. CI runs this target
permanently alongside the P1 gateway/security test.

The P3 controller still owns cross-generation cancellation, CR status and orphan
sweeping. Local operation serialization does not claim control-plane fencing or
EKS isolation guarantees.
