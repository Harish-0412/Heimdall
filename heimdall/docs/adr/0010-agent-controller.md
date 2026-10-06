# ADR 0010: The agent's controller, sources, sweeper and admission

Status: accepted for P3

## Context

P3 turns the P2 engine into a declarative, self-healing, outbound-only agent
(ADR 0004, ADR 0005). Engine operations take minutes (databases, migrations,
smoke tests); the agent can be killed at any point; several replicas may run;
desired state comes from `kubectl apply` now and from the control plane in P5;
and nothing may ever be deleted on the strength of a failed read.

## Decisions

**One CRD, spec as the whole intent.** `PreviewEnvironment`
(`heimdall.dev/v1alpha1`) in the agent's namespace. The spec carries the inline
`heimdall.yaml` with its SHA-256, digest-pinned images, identity and an
explicit deployment `generation`; nothing is inferred from labels or cluster
state. The API server enforces what it can with CEL: identity is immutable,
`generation` and `resetNonce` never decrease, and changed inputs require a
higher generation. `desiredState` accepts `Running` and `Destroyed` only
(`Sleeping` arrives with P8).

**Status as the record, written only by the agent.** `deployedGeneration`,
`completedResetNonce` and the last operation (with attempts and `retryAfter`)
are facts the reconciler decides from. A ValidatingAdmissionPolicy denies
status writes by anyone else: a forged record would make the agent skip work.

**Reconcile never blocks.** A reconcile compares spec with status and starts
at most one operation per environment on a runner (bounded concurrency,
cancellable contexts). Progress returns as channel events, so status streams
steps and per-stage conditions while the operation runs. Reads for decisions
go to the API server, not the cache, so a stale cache cannot repeat or skip
work.

**Fencing depends on the spec alone.** An operation is current while the spec
asks for exactly that work (type, generation, reset nonce, deletion). A
non-current operation is cancelled; a result that arrives anyway is recorded
as cancelled and discarded. Using status here created feedback loops (an
environment stuck queued after destroy-then-run); the spec-only rule removed
them. Within the engine, the journal lease (ADR 0009) serializes operations on
one environment across processes.

**Failures are classified once.** Engine and agent errors map to stable codes
and a retryable flag. Retryable failures back off exponentially (rounded up to
whole seconds, the precision status stores); terminal ones wait for new input.
An operation the agent did not see finish is `Interrupted` and resumed; a
journal lease held by a dead agent (`engine.busy`) is waited out at a steady
pace and is not reported as a failure. An unfinished apply of the current
generation is always resumed, including a repair of a `Degraded` environment.

**Pluggable desired-state sources.** A `Source` returns the complete desired
set or an error, never "empty" on failure. `cluster` treats the objects as the
truth; `configmap` and `file` read a strict `DesiredState` document whose
`environments` key is required (omitting it is an error, not "delete
everything"). For document sources a syncer server-side-applies the objects
with its own field manager and label, and deletes only objects carrying that
label that the document no longer names (UID-preconditioned). An admission
policy then reserves those objects for the agent. P5's control-plane source
implements the same interface.

**A fail-safe sweeper.** Deletes an orphaned namespace only when ownership
labels verify, it is older than the grace period, an earlier pass saw the same
UID orphaned, and both the source and the object list were read
authoritatively in the same pass. Any read failure means no deletions. Desired
= source ∪ existing objects, so the sweeper never races a destroy in progress.
Suspicion is in memory: a new leader starts over, which only delays deletion.

**Self-managed webhook certificates.** Each replica ensures a CA and serving
certificate (ECDSA P-256) in a chart-created Secret and injects the CA into its
own webhook configuration. RBAC is scoped to those two named objects. This
avoids a cert-manager dependency; the webhook fails closed and readiness waits
for the certificate.

**Enforcement before action.** Every replica verifies, by a server-side dry
run that must be denied, that its namespace policy is enforced before the
leader changes anything, and keeps checking. Readiness includes it.

**Leader election with every replica serving admission.** Only the leader
reconciles, syncs and sweeps; all replicas serve the webhook. Rolling updates
surge, so the fail-closed webhook stays available.

## Consequences

- Killing the agent at any point is safe and is tested on kind in every stage
  of an apply and during a destroy.
- The objects are the integration point: kubectl, GitOps and, in P5, the
  control plane all express intent the same way.
- Status writes and generated objects need the admission policies; with
  `admissionPolicy.enabled=false` those guarantees rest on RBAC alone.
- envtest does not build on Windows with controller-runtime v0.25; the suites
  run on Linux (CI) or in a container.
