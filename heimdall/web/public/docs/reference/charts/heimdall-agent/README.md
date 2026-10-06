# heimdall-agent

Installs the Heimdall agent in a cluster that runs pull-request preview
environments: the `PreviewEnvironment` CRD (`heimdall.dev/v1alpha1`), the
controller that creates, repairs and destroys previews from it, the orphan
sweeper, and the admission webhook that rejects invalid environments at
`kubectl apply`. This is the only Helm chart in Heimdall's data path: it ships
Heimdall itself. Previews are applied by the agent as typed objects, never as
Helm releases ([ADR 0007](../../docs/adr/0007-resource-ownership.md)).

The agent is **outbound-only** ([ADR 0004](../../docs/adr/0004-tenant-isolation-and-cluster-ownership.md)):
it talks to the Kubernetes API (and, from P5, pulls desired state from the
Heimdall control plane). Only the API server (calling the webhook) and a
metrics scraper connect in. How it works: [docs/agent.md](../../docs/agent.md)
and [ADR 0010](../../docs/adr/0010-agent-controller.md).

## Prerequisites

- Kubernetes 1.30+ (ValidatingAdmissionPolicy is GA).
- A NetworkPolicy-enforcing CNI (previews rely on default-deny policies).
- Gateway API CRDs and an implementation, with one shared `Gateway` for
  previews ([ADR 0008](../../docs/adr/0008-gateway-api-for-preview-routing.md);
  example: [`test/e2e/kind/gateway.yaml`](../../test/e2e/kind/gateway.yaml)).

## Install

```bash
helm install heimdall-agent charts/heimdall-agent -n heimdall-system --create-namespace \
  --set platform.baseDomain=preview.example.com \
  --set image.digest=sha256:<digest>
```

Then create a preview (the `heimdall manifest` command prints one from a
`heimdall.yaml`, with its digest and pinned images):

```bash
heimdall manifest heimdall.yaml --tenant acme --repo acme/shopflow --pr 184 \
  --images images.json | kubectl apply -f -
kubectl -n heimdall-system get previewenvironments -w
```

Values are validated by `values.schema.json` (unknown keys are rejected), and
`make helm-lint` checks that the configuration the chart renders is accepted
by the agent's own strict parser (`heimdall-agent --check-config`).

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `tag` / `digest` | `ghcr.io/heimdall-dev/heimdall-agent` / appVersion / none | Prefer `digest` in production |
| `replicaCount` | `1` | 1-3; replicas elect a leader, all serve the webhook. 2 gives fast failover and a PodDisruptionBudget |
| `source.type` | `cluster` | `cluster`: PreviewEnvironments are applied with kubectl. `configmap`: a `DesiredState` document in `source.configMap` (key `source.key`) is the truth and the agent writes the objects |
| `source.syncInterval` | `30s` | How often a document source is read |
| `platform.*` | | The cluster as previews see it; `platform.baseDomain` is **required** ([docs/rendering.md](../../docs/rendering.md)) |
| `policy.*` | private, no secrets | Tenant policy until P5 delivers it ([ADR 0006](../../docs/adr/0006-preview-config-trust-policy.md)) |
| `operations.maxConcurrent` / `stepTimeout` / `resyncInterval` / `maxBackoff` | `4` / `10m` / `5m` / `10m` | Engine work bounds, drift checks and retry cap |
| `sweeper.enabled` / `dryRun` / `interval` / `gracePeriod` | `true` / `false` / `5m` / `30m` | Orphan removal (see docs/agent.md) |
| `rbac.clusterWideRead` | `false` | Cluster-wide reads of preview workload kinds (unused by the P3 agent) |
| `rbac.aggregateToDefaultRoles` | `true` | `view` reads and `edit`/`admin` write PreviewEnvironments where bound |
| `admissionPolicy.enabled` | `true` | Confine the agent's writes; the agent waits until they are enforced |
| `webhook.port` / `failurePolicy` / `timeoutSeconds` | `9443` / `Fail` / `10` | Admission webhook |
| `metrics.port` / `secure` | `8443` / `true` | HTTPS metrics with Kubernetes authn/authz |
| `networkPolicy.enabled` / `metricsFrom` | `true` / any | Ingress only to the webhook and metrics ports |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | | Scheduling |

## Permissions

Everything the agent can do, and why. Three scopes keep powerful rights out
of the cluster-wide binding. `test/e2e/kind/agent-rbac.sh` checks every line
below on a live cluster, including the denials.

### Tier 1: `heimdall-agent` (ClusterRole, bound cluster-wide)

| Resource | Verbs | Why |
|---|---|---|
| `namespaces` | get, list, watch, create, update, patch, delete | Create and destroy preview namespaces; the sweeper lists them |
| `rolebindings` | get, create | Bind tier 2 inside each new preview namespace (never changed or deleted) |
| `clusterroles` (`heimdall-agent-preview-manager` only) | bind | Grant tier 2 without holding it anywhere |
| `validatingwebhookconfigurations` (`heimdall-agent` only) | get, update | Inject the webhook's CA |
| `tokenreviews`, `subjectaccessreviews` | create | Authenticate and authorize metrics scrapers (`metrics.secure`) |
| preview workload kinds | get, list, watch | Only with `rbac.clusterWideRead=true` |

Not granted cluster-wide: **secrets, config maps, pod logs, exec**, and any
write except namespaces and role bindings.

### Tier 2: `heimdall-agent-preview-manager` (ClusterRole, never bound cluster-wide)

Bound by the agent with a RoleBinding **inside each preview namespace only**.

| Resource | Verbs | Why |
|---|---|---|
| services, config maps, secrets, service accounts, PVCs, resource quotas, limit ranges | full | Rendered guardrails, credentials, import, database volume, the engine's operation journal |
| deployments, statefulsets, jobs | full | Workloads, dependencies, migrations, import, smoke tests |
| network policies, HTTPRoutes | full | Isolation and routing |
| pods | get, list, watch, delete | Status; removing pods a failed rollout leaves behind |
| pods/log, replicasets, endpointslices | get, list, watch | Status and diagnostics ([docs/diagnostics.md](../../docs/diagnostics.md)) |
| events | get, list, watch, create, patch | Timeline |

Deliberately absent: `pods/exec`, `pods/attach`, `pods/portforward`, RBAC
inside the namespace, and anything cluster-scoped.

### Own namespace: `heimdall-agent` (Role)

| Resource | Verbs | Why |
|---|---|---|
| `previewenvironments` (+ `/status`, `/finalizers`) | full; status get/update/patch | The desired state (cluster source) or the agent's copy of it; its records |
| `configmaps` | get | The desired-state document and approved imports, by name; never listed |
| `secrets` (`heimdall-agent-webhook-tls` only) | get, update | Its webhook CA and certificate. The chart creates the Secret empty, so the agent needs no `create` |
| `leases` | get, list, watch, create, update, patch | Leader election |
| `events` | create, patch | Its own events |

`heimdall-agent-metrics-reader` (ClusterRole, `get` on `/metrics`) is for you
to bind to your scraper's identity.

### Admission policies

RBAC cannot say "only namespaces you created", so three
**ValidatingAdmissionPolicies** do:

- `heimdall-agent-namespaces` (agent only): namespaces named `heimdall-*`
  **and** labelled `heimdall.dev/preview=true`, both before and after the
  change. The agent cannot adopt an existing namespace by labelling it, and
  cannot touch `kube-system` or its own namespace.
- `heimdall-agent-rolebindings` (agent only): role bindings only inside such
  namespaces, and only for `heimdall-agent-preview-manager`.
- `heimdall-agent-environments` (everyone **but** the agent): PreviewEnvironment
  status is the agent's record, so nobody else may write it (a forged
  `deployedGeneration` would make the agent skip work). With
  `source.type=configmap`, the objects are generated from the document, so
  nobody else may create, change or delete them either: edit the document.

**Policies take effect asynchronously** (observed right after `helm install`).
The agent therefore verifies enforcement before acting: a server-side dry run
creating an unlabelled namespace must be denied by `heimdall-agent-namespaces`.
Until then it is not ready and changes nothing; it keeps re-checking every
5 minutes (`heimdall_agent_admission_policy_enforced`).

## Security of the agent pod

Non-root (65532), distroless image, read-only root filesystem, all
capabilities dropped, seccomp `RuntimeDefault`, no privilege escalation.
HTTP/2 is disabled on the webhook and metrics servers. Its service-account
token is mounted because it talks to the Kubernetes API; preview pods never get
one. Rolling updates surge a new pod before stopping an old one, so the
fail-closed webhook stays served; only the elected leader reconciles.

## Verifying

```bash
make helm-lint kubeconform
kubectl auth can-i --list --as system:serviceaccount:heimdall-system:heimdall-agent
kubectl auth can-i --list -n heimdall-system --as system:serviceaccount:heimdall-system:heimdall-agent
```
