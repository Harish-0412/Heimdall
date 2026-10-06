# Completion review through Phase 4

Reviewed on 4 October 2026 against [the phase plan](phases.md),
[backend design](backend-design.md), and ADRs 0004–0011.

The workspace already contained the Phase 3 agent and most Phase 4 diagnostics.
The remaining work was correctness, safe evidence collection, complete fixtures,
and verification. The control plane, GitHub delivery, public gateway isolation,
and later lifecycle features remain in their planned phases, starting at P5.

## Gaps resolved

| Area | Finding and completed change |
|---|---|
| Output fixtures | Eight of the fifteen captured scenarios lacked JSON, CLI and PR-comment goldens; crash and quota expectations also differed from their snapshots. All fifteen now have all three reviewed outputs. |
| Current state | Historical journal failures survived successful retries. Old workload generations, rollout pods, reused object names, obsolete ReplicaSets and Gateway conditions could affect current diagnoses. These are fenced by generation, template, UID and observed generation. Cleared environment values are compared exactly; obsolete pods are removed before sanitization erases template differences. |
| Recovery | Recovered crashes, OOM kills and old liveness warnings could remain diagnoses. Current container state and start timestamps distinguish recovery from an active failure. |
| Useful evidence | Old logs and routine events could consume collection limits. Failed Jobs and crash reports could lose an initial error before a large dump. Collection prioritizes current warnings and preserves the first error through diagnosis. |
| Snapshot hygiene | Init containers, templates, arbitrary metadata, probe headers, route filters and other payloads were insufficiently sanitized. Retained fields are sanitized; unrelated payloads are removed. |
| Known secrets | Short values and URL-safe base64 were missed; splitting multiline text before redaction could expose private-key bodies. Every nonempty injected value is redacted, with common encodings, before truncation. Structural generation, stage and public object identifiers remain usable. |
| Unreadable secrets | Collection considered only two platform Secret names and omitted only logs on read failure. It now discovers pod references, including init/debug containers and volumes, and removes all free-form evidence when values are unavailable. Confirmed missing Secret names remain structured evidence; permission errors do not imply a missing Secret. |
| CLI | Imported snapshots are sanitized before saving or displaying; unexpected positional arguments are rejected. |
| Agent status | Diagnosis collection failures could leave no diagnosis. A fallback report retains the original failure and step; status output obeys all CRD limits. |
| Operation failure | Readiness symptoms could hide `engine.lock_lost`. Lost journal ownership is now the primary `UNCLASSIFIED` finding, with cluster/API recovery guidance and independent workload findings secondary. |
| Fail-fast watchdog | Old rollout pods could fail their replacement, and the watchdog could outlive its wait. Template matching and joined cancellation prevent both. |
| Journal ownership | Renewal errors lost their typed cause. Expired holders could attempt writes, and namespace deletion could outlive its lease. Writes, release and destructive requests are bounded by the existing lease; `engine.lock_lost` survives through events and controller classification. |
| Webhook certificates | A mismatched CA key could block renewal indefinitely; failed trust injection could leave readiness true. Both recover safely and have regression tests. |
| Crash-recovery evidence | Stage polling accepted later stages. A scoped test admission gate now holds each exact stage before the leader is killed; destroy is held on an actual Job deletion and resumed before release. Resumed attempts must have a strictly newer recorded start time, so timestamps in the same second cannot reuse old status. Test waits ignore the prior generation's error while the replacement is running. |
| Live database scenario | Waiting for one error string in a short log tail was unreliable after an API crash. The test now proves the real database-backed health route returns 200 before shutdown and fails or times out after PostgreSQL is gone, allowing the API to restart. Typed PostgreSQL availability remains enough to explain the failure when a driver never prints an error. |
| Build and CI | Tool versions and the agent image name were undefined in Make targets; `build-agent` was missing. Versions are pinned, targets work, diagnostics goldens are checked for drift, and failed envtest setup cannot silently skip the API-server suites. |

## Verification

Local checks completed on 4 October 2026:

- `go test -race -count=1 ./...` with Linux envtest assets, including the real
  API-server controller, CRD and source suites.
- `go vet ./...`, `go mod verify`, schema and render drift checks, and all fifteen
  captured diagnosis goldens.
- `golangci-lint run`: zero issues.
- Strict example validation; all 125 rendered resources and all 22 chart resources
  pass `kubeconform -strict`; Helm lint and both source configurations pass the
  agent's strict parser.
- Windows agent build, Linux amd64/arm64 image build, and the docs site build.
- The engine's real kind API contract suite: journal concurrency, generation
  fencing, field ownership, safe pruning and deletion deadlines.

All Phase 3 and Phase 4 live exit criteria now have passing evidence against
dedicated kind clusters and actual ShopFlow processes, across full runs and
focused reruns:

| Exit criteria | Passing evidence in the local `out/` directory |
|---|---|
| P3 admission, apply/rollout/reset/delete, failure diagnosis and recovery, N+1 cancellation | Passing subtests in `phase3-kind-final.log` |
| P3 exact-stage recovery after five apply kills and a held destroy kill, requiring a strictly newer attempt | `phase3-exact-recovery-verification.log` |
| P3 orphan grace period, no sweeping during source outage, ConfigMap creation/removal, authenticated metrics | `phase3-recovery-final-verification.log` |
| P4 all fifteen diagnosis scenarios, with expected codes and actionable messages | Thirteen passing cases in `phase4-kind-verification.log`, image-pull in `phase4-kind-rerun.log`, database-down in `phase4-database-warm-verification.log` |
| Final engine API contracts | `phase2-api-final-verification.log` |
| Final complete race/envtest suite and vet | `phase4-final-unit-verification.log` |
| Refreshed captured diagnostics and strengthened recovery helper regressions | `phase4-final-captured-verification.log`, `phase3-final-helper-verification.log` |
| Final lint and amd64/arm64 agent image | `phase4-final-lint-verification.log`, `phase4-final-image-verification.log` |

Initial full runs encountered local API contention, cold image downloads and
test observation issues. The affected cases were investigated and rerun
serially in a warm cluster after the test fixes. The table records passing
cases rather than claiming those initial full runs exited successfully.
Temporary test clusters and registry containers were removed after verification.

The diagnostics contract remains the fourteen planned public codes plus
`UNCLASSIFIED`. Each code has a captured scenario and actionable output.
