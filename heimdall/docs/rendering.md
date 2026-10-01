# Rendering: from heimdall.yaml to Kubernetes objects

`internal/render` turns a loaded `heimdall.yaml` plus the facts of one
deployment into typed Kubernetes objects (client-go types), grouped into
ordered stages. It is the contract between the config (P0) and the engine and
controller that apply it (P2, P3). `heimdall render` prints the same output.

```text
config.Load(heimdall.yaml, tenant policy) ─┐
                                           ├─> render.Render ─> Plan{Namespace, URLs, Stages[Steps[Objects]]}
render.Context{tenant, repo, PR, SHA,     ─┘
  generation, images, seed, platform...}
```

`Render` is a **pure function**: no cluster access, clock, randomness or file
system. The same inputs produce byte-identical YAML (a property test checks
this over hundreds of random configs). Anything time- or randomness-dependent
(`ExpiresAt`, `URLSuffix`, `Credentials`) is an explicit input.

## Stages and steps

Stages are the pipeline of [ADR 0007](adr/0007-resource-ownership.md). Each
stage holds ordered **steps**; a step's objects are applied together
(server-side apply, one field manager) and awaited together before the next
step: Deployments and StatefulSets until available, Jobs until complete.
Every plan lists all five stages; a stage with nothing to do has no steps.

| Stage | Steps | Objects |
|---|---|---|
| `guardrails` | `setup` | Namespace, ServiceAccount, credentials Secret, ResourceQuota, LimitRange, NetworkPolicies |
| `dependencies` | `start` | Postgres StatefulSet, Redis and RabbitMQ Deployments, their Services |
| `baseline-db` | `prepare` → `migrate` → `seed` → `clone` | DB scripts ConfigMap and Jobs (below) |
| `application` | `wave-1`, `wave-2`, ... | Services, Deployments, HTTPRoutes, in `dependsOn` order |
| `smoke` | `run` | one Job per smoke test, in parallel |

**Waves.** A workload is in the wave after the deepest *service* it depends on
(dependencies on postgres/redis/rabbitmq are already satisfied by then). For
ShopFlow: wave 1 is `api`; wave 2 is `web` and the `notifications` worker.

## Database lifecycle

```text
prepare  (superuser)  role `app`; DROP + CREATE app_baseline OWNER app
migrate  (app)        the PR's migration command, DATABASE_URL -> app_baseline
seed     (app)        psql --single-transaction -f seed.sql into app_baseline
clone    (superuser)  freeze app_baseline (no connections, template);
                      DROP + CREATE app TEMPLATE app_baseline
```

- Every generation rebuilds the baseline from scratch, so a preview reflects
  exactly its commit's migrations and seed. Workloads use `app`.
- **Trust boundary:** migrations and the seed are PR-controlled, so they run as
  the unprivileged `app` role (no superuser: no `COPY ... PROGRAM`, no file
  access). Only Heimdall's own scripts (`heimdall-db-scripts` ConfigMap) use
  the superuser.
- `reset` (P2) re-runs only `clone.sql` after suspending workloads.
- The seed travels in an **immutable, per-generation** ConfigMap
  (`heimdall-seed-g<N>`), at most 768 KiB. Larger fixtures will come from the
  OCI bundle the agent pulls (ADR 0004, P3).

## Naming

| Thing | Rule | ShopFlow PR 184 |
|---|---|---|
| Namespace | `heimdall-pr<N>-<repo>-<suffix>` | `heimdall-pr184-shopflow-x7d2` |
| Primary host | `pr<N>-<repo>-<suffix>.<base domain>` | `pr184-shopflow-x7d2.preview.example.com` |
| Other public hosts | `pr<N>-<repo>-<service>-<suffix>.<base domain>` | `pr184-shopflow-api-x7d2.preview.example.com` |
| Workload objects | the workload's name | `api`, `web`, `notifications` |
| Per-generation objects | `heimdall-<what>-g<generation>` | `heimdall-migrate-g3`, `heimdall-smoke-api-health-g3` |
| Platform objects | `heimdall-*` (workload names may not use the prefix) | `heimdall-quota`, `heimdall-default-deny` |

Hostnames are a **single DNS label** so the cluster's one wildcard certificate
covers every preview ([ADR 0008](adr/0008-gateway-api-for-preview-routing.md)).
`URLSuffix` is random per environment (4-8 of `[a-z0-9]`) and stable across
generations; it makes URLs hard to guess but is not an access control (P7 is).
Every name is a valid DNS label: when parts are too long the repository part is
shortened first, and truncated names get a hash suffix so they stay unique
(fuzz-tested).

## Labels and annotations

On every object. **Metadata only**: nothing reads them back to decide what
should exist ([ADR 0005](adr/0005-sources-of-truth-and-generation-fencing.md)).

| Label | Value |
|---|---|
| `heimdall.dev/tenant`, `heimdall.dev/env`, `heimdall.dev/pr` | tenant slug, environment id, PR number |
| `heimdall.dev/repo` | `owner.name` (exact `owner/name` in the annotation) |
| `heimdall.dev/generation` | deployment generation; pruning selects older values |
| `heimdall.dev/owner`, `heimdall.dev/expires` | PR author, expiry (Unix seconds) |
| `heimdall.dev/stage` | stage that created the object |
| `heimdall.dev/preview=true` | namespace only: the agent's admission scope and the Gateway's route selector |
| `app.kubernetes.io/{name,instance,component,part-of,managed-by}` | recommended labels; `name` is the selector |

Annotations: `heimdall.dev/sha` (all objects), `heimdall.dev/repo`,
`heimdall.dev/expires-at` (RFC 3339), `heimdall.dev/url`,
`heimdall.dev/visibility` (namespace and routes).

Pod templates of long-running workloads carry only labels that are stable for
the environment's life. Generation and expiry are on object metadata only, so a
new push does not restart Postgres and extending the TTL restarts nothing. Job
pod templates also carry generation and stage, for diagnostics (P4).

## Secure defaults

Rendered into every pod; `heimdall.yaml` cannot express or weaken them
([ADR 0006](adr/0006-preview-config-trust-policy.md)). One builder
(`security.go`) produces them all, and `TestSecurityDefaults_*` plus the
property test assert them for every pod of every scenario.

| Control | Setting |
|---|---|
| Kubernetes API access | dedicated `heimdall-workload` service account, `automountServiceAccountToken: false` on account and pod |
| User | `runAsNonRoot`, explicit UID (10001 for apps; the image's own unprivileged UID for Postgres/Redis/RabbitMQ/curl) |
| Privileges | `privileged: false`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, seccomp `RuntimeDefault` |
| Filesystem | `readOnlyRootFilesystem: true`, size-limited `emptyDir` at `/tmp` (and data dirs for dependencies) |
| Host | no host network/PID/IPC, host paths or host ports; `enableServiceLinks: false` |
| Images | pinned by digest only; never `latest`; `imagePullPolicy: IfNotPresent` |
| Secrets | never in pod specs: connection URLs and passwords come from `secretKeyRef` |
| Diagnostics | `terminationMessagePolicy: FallbackToLogsOnError` |
| Namespace | Pod Security Admission `restricted` (enforce, audit, warn) |

`Platform.WritableRootFilesystem` relaxes the read-only root for **application**
containers whose images cannot run without it; Heimdall's own containers stay
read-only. Apps that write elsewhere than `/tmp` (for example npm's cache)
should be pointed at `/tmp`, as the ShopFlow Dockerfile does.

Probes: readiness only (HTTP when `health` is set, TCP otherwise). A liveness
probe guessed for an unknown application restarts slow starters in a loop;
crash loops are diagnosed from restarts instead (P4).

## Resources

- Containers get **limits and requests**. Defaults (ADR 0007): CPU request 20%
  of the limit (floor 10m), memory request 50% of the limit (floor 64Mi),
  rounded up, never above the limit.
- **Size presets**: `small` 250m/256Mi, `medium` 500m/512Mi (service default),
  `large` 1 CPU/2Gi (needs `Policy.AllowLargeSize`). Workers default to
  `small`. A preset cannot be combined with explicit values.
- **Memory over-commit** is capped per container: the validator rejects a
  memory request below `MinMemoryRequestPercent` (default 50%) of the limit,
  and the LimitRange enforces the same ratio at admission.
- **ResourceQuota** = the tenant's ceilings (`MaxTotalCPUMilli`,
  `MaxTotalMemoryMi`) plus room for the largest step of concurrent Jobs;
  pods, PVCs and storage bounded by policy; **LoadBalancer and NodePort
  services forbidden** (previews are reachable only through the gateway).
- Deployments roll with `maxSurge: 0` so an update never needs more than the
  quota (a few seconds of unavailability on push, in exchange for a
  predictable footprint). Dependencies use `Recreate`.

## Network

Default deny, then exactly these allows:

| Policy | Allows |
|---|---|
| `heimdall-default-deny` | nothing (all ingress and egress denied) |
| `heimdall-allow-same-namespace` | pod-to-pod inside the preview |
| `heimdall-allow-dns` | egress to cluster DNS on 53/UDP+TCP (`Platform.DNSPeers`) |
| `heimdall-allow-gateway-<service>` | ingress from the gateway data plane to that public service's port only |
| `heimdall-allow-egress` | only when `Platform.EgressCIDRs` is set; cloud metadata ranges (`169.254.0.0/16`, `fd00:ec2::254/128`) are always excepted, and an allowlist entry inside them is rejected |

So a preview cannot reach other previews, the Kubernetes API, the control
plane, instance metadata or the internet. (The kind e2e verifies ingress and
egress denial live; EKS-specific proof is P9/P10.)

## Environment variables

| Variable | Workloads | Value |
|---|---|---|
| `PORT` | services | the declared port |
| `HEIMDALL_PR`, `HEIMDALL_SHA` | all, smoke tests | PR number, full commit SHA |
| `HEIMDALL_PUBLIC_URL` | all, smoke tests | primary service URL (if any) |
| `HEIMDALL_PUBLIC_URL_<SERVICE>` | all, smoke tests | each public service, upper snake case (`my-api` → `MY_API`) |
| `DATABASE_URL` | all workloads; migrations get the baseline DB | from `heimdall-credentials` |
| `REDIS_URL`, `AMQP_URL` | all workloads | from `heimdall-credentials` |
| your `env` and `secrets` | as declared | secrets from `heimdall-app-secrets` |

Order is fixed (injected first, then `env` sorted, then `secrets` sorted), and
`heimdall.yaml` may not redefine an injected name (`env.reserved`). Frontends
should read URLs at **runtime** (ShopFlow's storefront serves them from
`/config.js`), so one image works in every preview.

## Images

Application images come from `Context.Images` (built by CI, pushed by digest)
or from an `image:` already pinned in `heimdall.yaml`. A tag-only reference is
rejected (`render.image.missing`/`unpinned`), as is `latest`
(`render.image.latest`); a resolved image must be the repository declared in
`heimdall.yaml` (`render.image.mismatch`), since policy checked that one.

Heimdall's own images (Postgres, Redis, RabbitMQ, the curl toolbox for smoke
tests) are pinned in `internal/render/catalog.go` by multi-arch index digest,
from mirrors without anonymous pull limits (ECR Public's copy of Docker
Official Images, quay.io for curl). `TestCatalog` keeps the catalog and the
validator's supported versions in step. `Platform.ImageMirror` redirects them
to a pull-through cache; digests still pin the content. To update a pin:
`docker buildx imagetools inspect <ref>` and copy the index digest.

> RabbitMQ 3.12/3.13 are past upstream community support; adding 4.x (and
> Postgres 18) is a catalog entry plus a supported-version entry, best done
> with an e2e run for each.

## Platform settings

`Context.Platform` describes the cluster; only the operator sets it.

| Field | Default | Purpose |
|---|---|---|
| `BaseDomain` | required | zone covered by the wildcard certificate |
| `URLScheme`, `URLPort` | `https`, none | URL form (`http` and a port for local clusters) |
| `Gateway` | `heimdall-gateway/heimdall` | shared Gateway routes attach to |
| `IngressPeers` | the Gateway's namespace | who may reach public services |
| `DNSPeers` | kube-dns in kube-system | where pods resolve names |
| `EgressCIDRs` | none | destinations outside the cluster |
| `StorageClassName` | cluster default | database volume |
| `NodeSelector`, `Tolerations` | none | dedicated preview node pool (P7) |
| `RuntimeClassName` | none | sandboxed runtime, for example gVisor |
| `ImageMirror` | none | registry for Heimdall's own images |
| `WritableRootFilesystem` | false | relax read-only root for app containers |

## Using `heimdall render`

```bash
# Inspect what a config produces (placeholder images are not runnable).
heimdall render --placeholder-images --stage guardrails
heimdall render --placeholder-images --list            # namespace, URLs, steps

# A real deployment: pinned images, credentials, one file per step.
heimdall render --image api=ghcr.io/acme/api@sha256:... --image web=... \
  --generate-credentials --out-dir out/ heimdall.yaml
for f in out/*.yaml; do kubectl apply --server-side -f "$f"; done   # plus waiting, see test/e2e/kind/run.sh
```

Context flags (`--tenant`, `--repo`, `--pr`, `--sha`, `--generation`, ...)
have local defaults; in production the control plane supplies them. The seed
is read through `os.Root`, so a symlink cannot point outside the repository.
`--generate-credentials` writes passwords into the guardrails file, which is
created with mode 0600.

## Notes for the engine (P2)

- **Apply** each step with server-side apply and one field manager; wait as
  described above; record per-step timings (the e2e prints a baseline).
- **Prune** after a generation succeeds: delete objects in the namespace whose
  `heimdall.dev/generation` is older (Jobs, seed ConfigMaps, removed
  workloads). Jobs have no TTL on purpose, so the engine can still inspect a
  failed Job; pruning is what removes them.
- **Immutable fields**: a StatefulSet's `volumeClaimTemplates` (Postgres
  storage size) cannot change in place. Since the baseline is rebuilt every
  generation anyway, delete and recreate the StatefulSet when that apply is
  rejected. Jobs are immutable too, hence per-generation names.
- **Credentials**: generate once per environment (`GenerateCredentials`), keep
  them in the cluster's `heimdall-credentials` Secret, and pass them back into
  every render so the rendered Secret is unchanged.
- `heimdall-app-secrets` is delivered by the platform (External Secrets, P7);
  render only references it.

## Testing

| Layer | What | Where |
|---|---|---|
| Golden files | full output for ShopFlow, a minimal app and each optional feature | `internal/render/testdata/golden` (`make golden` to update) |
| Security defaults | every pod of every scenario; the checker itself is tested against tampered plans | `security_test.go` |
| Property | 400 random configs: determinism, structure, wave order, security | `property_test.go` |
| Fuzzing | name and label sanitizers | `make fuzz` |
| Schemas | every rendered object and the agent chart, strict | `make kubeconform` |
| End to end | ShopFlow on kind through a real Gateway, live security probes, idempotent re-apply | `make e2e-kind` |
