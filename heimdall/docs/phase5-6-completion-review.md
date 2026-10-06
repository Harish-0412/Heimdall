# P5 and P6 completion review

Reviewed against [phases.md](phases.md), [backend design](backend-design.md) and
ADRs 0004–0007. This document records the implemented behavior and distinguishes
local acceptance from the required live GitHub acceptance.

**P5:** **DONE**, verified on 2026-10-05. The final expanded real kind acceptance
passed in 538.239 seconds with shell exit 0. It exercised approved synthetic
fixture transport and ownership, create/retry/reset, two isolated previews,
application data preservation on expiry extension, first-build failure diagnosis
and cleanup, credential/replica restart, on-demand logs, API outage preservation
and deletion after tenant policy tightening. Linux regression and real-service
integration also passed with race detection.

**P6:** implemented and locally verified; **live acceptance pending**. The user
authorized `Harish-0412/Startup-Assisstant` and requested preparation of a new
Heimdall App. No runtime App has been registered/installed in this session. The
connected Codex GitHub App cannot provide the Heimdall runtime's credentials.

## What these phases add

P0–P4 provide the configuration, trust checks, secure renderer, Kubernetes
lifecycle engine, agent/controller and diagnostics. P5 gives that system a
durable coordinator: the API stores desired state and policy, accepts scoped
agent observations, exposes a stage timeline, and retains deployment/audit
history after preview deletion. The customer agent continues to execute the
same engine through its outbound connection.

P6 connects GitHub to that coordinator. Signed events are queued in PR order,
the orchestrator checks canonical current state, trusted Actions deliver
immutable build digests, and the agent converges the matching generation.
The outbox edits the same App-owned PR comment/check and authorizes commands
using current repository permissions. Closing one PR requests cleanup of only
its environment.

P5 delivers the durable notification outbox infrastructure, and P6 supplies its
GitHub dispatcher. The Slack adapter remains the post-MVP Slack app work in P12;
Slack delivery is not claimed by these phases.

## Requirement and use-case coverage

| Contract or use case | Implementation | Verification |
|---|---|---|
| Persistent environment/deployment history, stage events, diagnoses and smoke results | `internal/domain`, `internal/store`, Goose migrations and sqlc contracts | Real PostgreSQL state-machine/CAS/immutable-history/stage-dedup tests |
| Cross-tenant isolation and missing tenant context | Non-owner application role, privilege verification, `FORCE RLS`, transaction-local authenticated tenant | Adversarial real PostgreSQL queries, API authentication/scope tests |
| Current generation wins over old CI/runtime work | Canonical PR version, immutable committed spec, generation fence, status event dedup and audited stale results | Real PostgreSQL tests; duplicate/out-of-order webhook and old callback cases |
| API retry/reset/delete, idempotency and keyset/SSE | Generated OpenAPI, strict JSON, CAS version, durable event cursors and repeated authorization | HTTP contract/unit tests; real database lifecycle and SSE revocation tests; kind lifecycle |
| Short-lived cluster identity and replica/restart recovery | Hash-only credentials, stable derivation key, bounded nonce replay, shared Secret CAS | Lost-response/write/restart and concurrent-rotation tests; cluster revocation; kind replica restart |
| Policy updates and reduced ceilings | Persistent explicit policy, revision bump, every-replica warm-up, bounded freshness and admission checks | HTTP/store tests, source/controller tests and real kind cleanup after tightening |
| API outage cannot become empty intent | Authoritative source failure, fail-safe sweeper, version/generation-preserving reconnect | Source tests; real kind outage extending beyond sweeper grace |
| Expiry extension preserves preview data | Version/revision metadata update, unchanged deployment generation and execution digest | Engine/controller behavior review; expanded kind order-preservation assertion |
| Requested logs across API replicas | Scoped current-generation workloads, secret redaction, UTF-8-safe 16 KiB budget, Redis atomic bounds/TTL, persistence verification | Unit scope/budget tests, real Redis two-server/TTL/concurrency tests; kind tail request |
| Configuration and fixture transport stays in customer registry/cluster | Reproducible contained OCI bundle, digest/hash/layer/path validation, exact sanitised-data attestation | Bundle security tests, source fixture ownership/pruning tests; kind actual registry pull |
| First-build artifact failure is visible and isolated | Generation-scoped safe preparation diagnosis, automatic retry/recovery, cleanup independent of artifact fetch | Source/Reporter regressions; expanded kind missing-bundle diagnosis/delete |
| Own App least privilege and safe retry | RSA App JWT, per-use scoped installation tokens, ETags, rate handling and recovery markers | Fake GitHub HTTP tests including ambiguous creation responses and forged markers |
| Signed webhook HTTP/Lambda boundary | Constant-time raw HMAC, caps, base64-preserving Lambda adapter, FIFO repository/PR identity | Recorded HTTP/Lambda fixtures, invalid signature/size tests |
| FIFO ordering, durable replay and dead letters | Encrypted AWS FIFO/DLQ template, inbox leases, payload identity and atomic intent/outbox commit | Real LocalStack plus PostgreSQL ordering/dedup/redrive/isolation tests |
| PR open/sync/reopen/close/merge, concurrent newer head | Canonical current GitHub reads, PR CAS and committed generation contract | Orchestrator tests; real queue replay of old open after close |
| Forks and unapproved configuration are refused | Fork guard before build credentials, default-branch baseline and current tenant policy | Fork/trust tables, exact full-head approval tests, OIDC head/run binding |
| Maintainer commands and non-collaborator denial | Current comment and write/maintain/admin permission verified through GitHub; audited command/denial | Command authorization tables; real-repository denial still pending |
| Trusted reusable CI and immutable artifact delivery | Commit-pinned workflow/tools, separate checkout, isolated build arguments, preview-only OIDC ECR IAM, vulnerability scan and digest callback | Workflow/script validation, App OIDC/run tests and build-policy tables; live Actions delivery pending |
| One edited PR comment and App-owned check per generation | Durable outbox, stable ownership markers, remote recovery and generation lock | Crash/retry/forged marker tests; actual GitHub rendering pending |
| Trace context across asynchronous boundaries | Bounded W3C identity, no baggage/source data, webhook/queue/orchestrator/desired projection/controller spans | Trace propagation/context tests; exporters are P10 work |

## Review fixes

- Preserved the prior committed spec while a replacement build is pending.
- Made cleanup work when policy has become stricter, configuration is missing,
  or the artifact registry is unavailable.
- Added central diagnosis for preparation failures before a CR exists.
- Normalized omitted diagnosis evidence to an empty JSON array so failures
  before any workload exists commit atomically instead of remaining Pending.
  Real PostgreSQL regressions cover omitted, empty and populated evidence,
  duplicate-report recovery and subsequent deletion.
- Bound imported fixture ConfigMaps to the exact owner UID; retained old data
  during rollout and rejected foreign-owner deletion.
- Required exact environment identity and source ownership for log requests,
  and aligned the redacted tail with the API's response limit.
- Parsed artifact/build syntax independently of local default deployment limits;
  current tenant policy still authorizes every actual deployment.
- Made Redis persistence settings verifiable so TTL-expired logs cannot survive
  in snapshots or append-only files.
- Added active verification/reusable workflows at the Git root and distinct
  bootstrap/application database connections.

## Reproduce verification

```bash
go test ./...
go vet ./...
make envtest test
make test-integration
make helm-lint kubeconform
make e2e-control
```

Linux Go 1.26 with real envtest binaries is required for the full controller/API
server regression run; Windows-only tests intentionally cannot prove those
cases. Integration-tag tests require Docker and fail when dependencies are
unavailable. CI requires real PostgreSQL, Redis, LocalStack and kind tests.

Local evidence is saved under `out/` (ignored build/test artifacts). Final runs
on 2026-10-05 are recorded below and can be reproduced through the commands
above and the root CI workflow. These records prove local/cluster acceptance;
they do not substitute for the live GitHub criterion.

| Verification | Result | Evidence in `out/` |
|---|---|---|
| Final expanded API-only kind acceptance with two outbound agent replicas | PASS, 538.239 s, shell exit 0 | `phase5-control-verification.log` |
| Linux Go 1.26 module verification, full vet and all-package race tests with real envtest | PASS | `phase5-6-linux-regression.log` |
| Linux Go 1.26 PostgreSQL, Redis and LocalStack integration with race detection | PASS: store 45.984 s, API 12.733 s, orchestrator 20.066 s | `phase5-6-linux-integration-verification.log` |
| Final PostgreSQL diagnosis evidence regression and full store/API service suites | PASS, omitted/empty/populated evidence and replay/deletion | `phase5-service-verification.log` |
| Signed ingress, canonical reconciliation, OIDC, queue replay, commands and GitHub outbox | PASS, including real PostgreSQL/LocalStack | `phase6-github-verification.log` |
| Strict Helm/config checks, JSON schema/goldens/CRD/deep-copy reproducibility and Kubernetes schema validation | PASS, 69 chart and 125 example resources, zero invalid/errors/skips | `phase5-manifest-verification.log` |
| Pinned sqlc generation repeated with identical file hashes | PASS | `phase5-generated-contract-verification.log` |
| Final operator example decoding and generated SQL compilation on Linux with race detection | PASS | `phase5-bootstrap-linux-verification.log` |
| Full lint and integration-store vet | PASS, zero lint issues | `phase5-6-lint-verification.log` |
| Control-plane image for Linux AMD64 and ARM64, non-root runtime and ARM64 executable startup | PASS | `phase5-control-image-verification.log` |
| Actual Compose API readiness and operator cluster/enrollment/revocation workflow | PASS | `phase5-local-runtime-verification.log` |
| Static Linux ARM64 Lambda ZIP, executable mode/CRC and repeatable LocalStack FIFO/DLQ bootstrap | PASS | `phase6-release-artifact-verification.log` |
| Concrete own App/CLI/Actions-variable setup and runtime flag consistency | PASS, configuration prepared; actual registration pending | `phase6-setup-usability-verification.log` |
| Canonical/public document synchronization, TypeScript, production web build and desktop/mobile page/documentation checks | PASS, four browser checks in 44.5 s | `phase5-6-web-verification.log` |

The final acceptance agent was pinned to
`sha256:ecf69377a29cd94ed9429e74114c1c3afe6cb58b7d0f05d3e8c09a145ea2c0f2`.
Failed intermediate runs remain separate diagnostic artifacts; only the final
successful run above establishes P5 completion.
The disposable kind cluster and registry were removed after verifying no preview
namespaces or imported fixture ConfigMaps remained. Local Compose verification
services were stopped; the database volume and private operator output files
were retained for local setup. No customer cloud resources were provisioned.

## Remaining live acceptance

Follow [GitHub App setup](github-app-setup.md) to register/install the own App
on the authorized repository, supply the local private-key/webhook secret
configuration, expose reachable HTTPS ingress, and configure the reviewed
workflow and customer preview registry OIDC role. Then use two disposable PRs
to prove creation/isolation, update A, denied non-collaborator command, cleanup
A while B remains, and final cleanup. Record actual PR URLs and timestamps here.

P6 is **not DONE** until this real acceptance passes. No live repository change,
App registration, cloud provisioning or acceptance result is claimed by the
local tests. P7–P11 remain separate phases; gateway access, automatic TTL/cost
management, real EKS security and launch dashboard are outside this completion.
