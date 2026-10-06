# ADR 0009: Local engine, operation journal and data provenance

Status: accepted for P2

The renderer owns desired Kubernetes objects. The engine owns execution ordering,
readiness, retries and cleanup. Its infrastructure port permits P3 to reuse the
same operations without putting CLI concerns in reconciliation.

For local mode, the CLI supplies an explicit saved Spec. A resourceVersion-based
ConfigMap journal serializes mutations and records accepted generation, content
digest, reset nonce and bounded stage evidence. Labels scope ownership only;
neither labels nor the journal manufacture desired configuration. The cluster's
API identity and exact allowed kube-context are checked independently.

We use per-step watches with deadlines, reject competing field managers, retain
failed Jobs, and prune only after successful smoke checks. A Postgres storage
change rebuilds the StatefulSet and PVC with workloads quiesced; preview writes
are disposable. This handles shrinking as well as growing without depending on
StorageClass online expansion support.

The implementation requirement prohibits synthetic data and supersedes the
earlier roadmap wording permitting synthetic seeds. Defaults are schema-only.
Optional imports require an operator attestation tied to exact bytes, asserting
sanitisation and recording an approver and reason. The system does not download
production datasets or attempt to disguise generated data as real records.

Reset deliberately suspends APIs and workers, then restores the baseline and
recreates cache/broker state before resuming. The original zero-error load-test
criterion cannot hold while no API endpoints exist. P2 guarantees drained
destructive operations and verified recovery; a future gateway can supply a
maintenance response or buffer requests.

Normal deletion preserves finalizers. Break-glass requires Kubernetes-granted
administrative permissions, a reason, and a successful evidence write before
finalizer edits. It refuses unknown ownership and refuses namespace finalization
until discovery proves there are no remaining objects.

Local audit evidence is bounded. Central audit retention, authoritative desired
state, CR reconciliation and distributed control-plane fencing remain P3/P5.

The completion review tightened the journal lease boundary: failed renewals
cancel work with `engine.lock_lost`, and an expired holder cannot write or
renew. Final namespace deletion is requested within the remaining lease;
the subsequent deletion watch may continue as a read-only operation. Watchdog
goroutines are joined before a readiness wait returns.
