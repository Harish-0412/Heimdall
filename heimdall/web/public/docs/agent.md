# The agent: PreviewEnvironment controller

The agent (`cmd/agent`, installed by [`charts/heimdall-agent`](../charts/heimdall-agent/README.md))
runs in each cluster that hosts previews. It turns `PreviewEnvironment`
objects into running previews with the same engine as `heimdall up`
([engine.md](engine.md)), keeps them healthy, and guarantees cleanup. The
design decisions are in [ADR 0010](adr/0010-agent-controller.md).

```text
 desired state                       agent (leader)                          cluster
 ─────────────                       ──────────────                          ───────
 kubectl apply ──► PreviewEnvironment ─► reconciler ─► runner ─► engine ─► heimdall-<pr>-<repo>-<suffix>
 or DesiredState     (heimdall-system)     │  status ◄── steps ◄─┘           namespaces
 document ─► syncer ─┘                     └─ sweeper: orphans (grace, 2 passes, authoritative read)
                       admission webhook (every replica): same checks as the agent
```

## Quick start

```bash
helm install heimdall-agent charts/heimdall-agent -n heimdall-system --create-namespace \
  --set platform.baseDomain=preview.example.com --set image.digest=sha256:<digest>

# images.json maps every service and worker to a digest-pinned image (CI builds them).
heimdall manifest heimdall.yaml --tenant acme --repo acme/shopflow --pr 184 \
  --owner octocat --images images.json | kubectl apply -f -
kubectl -n heimdall-system get previewenvironments -w
```

```text
NAME                  PHASE          GENERATION   DEPLOYED   URL                                      AGE
acme-shopflow-pr184   Provisioning   1                                                                5s
acme-shopflow-pr184   Ready          1            1          https://pr184-shopflow-1f3a.preview...    2m
```

| To | Do |
|---|---|
| Roll out a change (commit, config, images) | `heimdall manifest ... --generation 2 \| kubectl apply -f -` |
| Reset the data to its baseline | same generation, higher `--reset-nonce` |
| Stop it but keep the record | `--destroyed` (`spec.desiredState: Destroyed`); apply without it to bring it back |
| Remove it | `kubectl delete previewenvironment <name>`: the finalizer destroys everything first |

## The object

`spec` is the whole intent; nothing is inferred from labels or the cluster.

| Field | Meaning |
|---|---|
| `tenant`, `repository`, `pullRequest`, `environmentID`, `urlSuffix` | Identity; **immutable** |
| `commit`, `owner`, `expiresAt` | Metadata (TTL enforcement arrives in P8) |
| `generation` | Deployment generation ([ADR 0005](adr/0005-sources-of-truth-and-generation-fencing.md)). Never decreases; must increase whenever `commit`, `config`, `images` or `data` change. The API server enforces both (CEL) |
| `config.inline` + `config.sha256` | The `heimdall.yaml`, and its SHA-256 (a mismatch is rejected) |
| `images` | Workload to digest-pinned image |
| `data` | Optional operator-approved, sanitised import from a ConfigMap in the agent's namespace ([ADR 0009](adr/0009-local-engine-and-data-provenance.md)) |
| `desiredState` | `Running` or `Destroyed`. `Sleeping` is rejected until P8 |
| `resetNonce` | Reset whenever it increases; never decreases |

`status` is the agent's record and only the agent may write it (admission
policy):

| Field | Meaning |
|---|---|
| `phase` | `Pending`, `Queued`, `Provisioning`, `Ready`, `Degraded`, `Resetting`, `Failed`, `Destroying`, `Destroyed` |
| `conditions` | `Ready`, `Progressing`, and one per stage: `GuardrailsReady`, `DependenciesReady`, `BaselineDatabaseReady`, `ApplicationReady`, `SmokeTestsPassed` |
| `observedGeneration`, `deployedGeneration`, `completedResetNonce` | What the agent has seen and what is actually deployed |
| `operation` | The current or last operation: type, generation, result, attempts, `retryAfter` |
| `steps` | Each engine step with state and duration |
| `lastError` | Stable `code`, message, step, whether it is retryable |
| `diagnoses` | Why the last operation failed and what to do: ranked codes, summaries, suggestions, evidence for the root cause ([diagnostics.md](diagnostics.md)) |
| `namespace`, `urls` | Where it runs and how to open it |

## How it behaves

- **Level-triggered.** Each reconcile compares `spec` with `status` and starts
  at most one operation: destroy if deleting or `Destroyed`; apply if the
  deployed generation is behind, the environment is `Degraded`, or an apply of
  this generation did not finish; reset if the nonce is ahead. The engine is
  idempotent, so re-running an operation is always safe.
- **Never blocks.** Operations run in the background with a concurrency limit
  (`operations.maxConcurrent`); their progress is streamed into `status.steps`
  and the stage conditions as it happens.
- **Generation fencing.** An operation's result counts only while the spec
  still asks for exactly that work. A newer generation, a reset or deletion
  cancels the running operation; a result that arrives anyway is discarded
  (event `StaleResultDiscarded`, metric `heimdall_agent_stale_results_total`).
- **Explained.** A failed operation is diagnosed before it is reported: the
  ranked findings go to `status.diagnoses`, the root cause to the `Ready`
  condition's message and a `Diagnosed` event. Workloads that cannot recover
  (a crash loop, repeated out-of-memory kills, an image that cannot be pulled)
  stop the operation early instead of waiting out the step timeout.
- **Retries.** Retryable failures back off exponentially (base 10s, capped at
  `operations.maxBackoff`). Failures that need new input (an invalid config, a
  failed migration, a missing image) are terminal for that generation: the
  environment stays `Failed` with the code until the spec changes.
- **Self-healing.** Ready environments are re-checked every
  `operations.resyncInterval`; missing or unready workloads make it `Degraded`
  and the same generation is re-applied.
- **Crash-safe.** An operation the agent did not see finish (it was killed,
  or lost leadership) is marked `Interrupted` and resumed. If the dead
  agent's journal lease still holds the environment, the agent waits for it to
  lapse (at most a minute; `lastError.code: engine.busy`) without reporting a
  failure.
- **Guaranteed cleanup.** The finalizer `heimdall.dev/environment` is removed
  only after a destroy succeeded, so a deleted object never leaves a
  namespace behind.

## Desired-state sources

| `source.type` | Truth | Who writes PreviewEnvironments |
|---|---|---|
| `cluster` (default) | The PreviewEnvironment objects | You (`kubectl apply`, GitOps) |
| `configmap` | A `DesiredState` document in a ConfigMap | The agent, by server-side apply, labelled `heimdall.dev/source=configmap`. Everyone else is denied (admission policy) |

P5 replaces both with the control plane's API, behind the same interface.

```yaml
apiVersion: heimdall.dev/v1alpha1
kind: DesiredState
revision: "42"            # optional, for logs
environments:             # required; [] means "none", omitting it is an error
  - name: acme-shopflow-pr184
    spec: { ...PreviewEnvironment spec, exactly as heimdall manifest prints it... }
```

A document is decoded strictly (unknown fields and duplicate names are
errors) and capped at 4 MiB. An unreadable or invalid document changes
nothing: the agent keeps what exists and reports `heimdall_agent_source_up 0`.

## The sweeper

Removes orphaned preview namespaces: ones no desired state accounts for (an
object deleted with its finalizer forced off, an agent replaced mid-destroy).
A namespace is deleted only when, in the same pass, **all** hold:

1. ownership verifies: `heimdall-` prefix, `heimdall.dev/preview=true`,
   `app.kubernetes.io/managed-by=heimdall`, and tenant/repo/PR/environment
   labels;
2. it is older than `sweeper.gracePeriod`;
3. an earlier pass already found the same namespace (same UID) orphaned;
4. the desired state **and** the PreviewEnvironment list were both read
   authoritatively (from the API server, not a cache).

If either read fails, the pass deletes nothing
(`heimdall_agent_sweeper_passes_total{result="source_unavailable"}`).
`sweeper.dryRun=true` only reports (`heimdall_agent_sweeper_orphans_total{action="dry_run"}`).

## Admission

The webhook (every replica) loads the inline config with the tenant policy and
renders it, exactly as the agent will before deploying, so an invalid config,
digest mismatch or unpinned image is rejected at `kubectl apply` with the
field and the reason. It fails closed. The agent issues its own CA and serving
certificate (ECDSA P-256; CA 10 years, certificate 1 year, renewed 30 days
before expiry, CA kept across renewals), stores them in the chart's
`heimdall-agent-webhook-tls` Secret, and injects the CA into the webhook
configuration. No cert-manager needed.

The agent also refuses to act until its own ValidatingAdmissionPolicies are
observably enforced (chart README, "Admission policies").

## Operating it

| Endpoint | |
|---|---|
| `:8081/healthz`, `:8081/readyz` | Ready = webhook certificate trusted and admission policy enforced |
| `:8443/metrics` | HTTPS; bearer token whose identity may `get /metrics` (bind `heimdall-agent-metrics-reader`) |

| Metric | |
|---|---|
| `heimdall_agent_environments{phase}` | PreviewEnvironments by phase |
| `heimdall_agent_operations_total{type,result}`, `heimdall_agent_operation_duration_seconds` | Operations |
| `heimdall_agent_step_duration_seconds{step}` | Engine steps of successful operations |
| `heimdall_agent_stale_results_total` | Results discarded by fencing |
| `heimdall_agent_source_up`, `_environments`, `_failures_total` | Desired-state source |
| `heimdall_agent_sweeper_passes_total{result}`, `heimdall_agent_sweeper_orphans_total{action}` | Sweeper |
| `heimdall_agent_admission_policy_enforced` | 1 once the policy is enforced |

Logs are JSON (`logLevel`); every reconcile line carries tenant, repo, PR,
environment id and generation. Events on each PreviewEnvironment: `Started`,
`Succeeded`, `Failed`, `Diagnosed`, `Rejected`, `Superseded`,
`StaleResultDiscarded`, `Degraded`.

Leader election (`heimdall-agent.heimdall.dev` Lease) lets 2-3 replicas run:
one reconciles, all serve the webhook. A killed leader is replaced within the
lease duration (15s) and its operations resume.

## Development

```bash
make build-agent docker-agent
make envtest     # controller, CRD rules and syncer against a real API server (Linux/macOS)
make generate    # deep copies and the CRD after changing internal/api
make e2e-agent   # the P3 exit criteria on kind (Docker, kind, kubectl, helm)
```

`HEIMDALL_E2E_RUN` accepts a Go test `-run` pattern for a focused rerun; without
it, the runner checks every Phase 3 exit criterion. Crash tests verify the exact
running stage through a scoped test admission gate before killing the leader.

On Windows, run envtest in a container (controller-runtime's envtest does not
build there). The container downloads its own API-server binaries, so no
preinstalled Linux tools are needed:

```bash
docker run --rm -v "${PWD}:/src" -w /src \
  -v heimdall-gocache:/root/.cache/go-build \
  -v heimdall-gomodcache:/go/pkg/mod public.ecr.aws/docker/library/golang:1.26 \
  sh -ec 'assets=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 --bin-dir /tmp/envtest -p path); test -n "$assets"; export KUBEBUILDER_ASSETS="$assets"; go test -race -count=1 ./internal/controller ./internal/api/... ./internal/source'
```

The agent can run outside a cluster against a kubeconfig with
`source.type: file` (a `DesiredState` document on disk) and `POD_NAMESPACE`
set; `heimdall-agent --check-config --config agent.yaml` validates a
configuration file and exits.
