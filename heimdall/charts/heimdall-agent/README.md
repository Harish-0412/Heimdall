# heimdall-agent

Installs the Heimdall agent in a cluster that runs pull-request preview
environments. This is the only Helm chart in Heimdall's data path: it ships
Heimdall itself. Previews are applied by the agent as typed objects, never as
Helm releases ([ADR 0007](../../docs/adr/0007-resource-ownership.md)).

The agent is **outbound-only** ([ADR 0004](../../docs/adr/0004-tenant-isolation-and-cluster-ownership.md)):
it connects to the Heimdall control plane, pulls desired state, and pushes
status. Nothing connects in.

> Status: the chart is complete; the agent binary arrives in phase 3. Until
> then the chart is validated by `helm lint`, schema checks and `kubeconform`
> in CI.

## Prerequisites

- Kubernetes 1.30+ (ValidatingAdmissionPolicy is GA).
- A NetworkPolicy-enforcing CNI (previews rely on default-deny policies).
- Gateway API CRDs and an implementation, with one shared `Gateway` for
  previews ([ADR 0008](../../docs/adr/0008-gateway-api-for-preview-routing.md);
  example: [`test/e2e/kind/gateway.yaml`](../../test/e2e/kind/gateway.yaml)).

## Install

```bash
kubectl create namespace heimdall-system
kubectl -n heimdall-system create secret generic heimdall-agent-credentials --from-literal=token=<cluster token>
helm install heimdall-agent charts/heimdall-agent -n heimdall-system \
  --set controlPlane.url=https://api.heimdall.dev \
  --set controlPlane.credentialsSecret=heimdall-agent-credentials \
  --set image.digest=sha256:<digest>
```

Values are validated by `values.schema.json`; unknown keys are rejected.

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `tag` / `digest` | `ghcr.io/heimdall-dev/heimdall-agent` / appVersion / none | Prefer `digest` in production |
| `controlPlane.url` | `""` | Heimdall API (HTTPS) |
| `controlPlane.credentialsSecret` | `""` | Secret with the cluster credential under `token` |
| `clusterName` | `""` | Name shown in the dashboard |
| `previewNamespacePrefix` | `heimdall-` | Must match the agent's naming |
| `rbac.clusterWideRead` | `true` | See "Permissions" |
| `admissionPolicy.enabled` | `true` | Confine writes to preview namespaces |
| `networkPolicy.enabled` / `metricsFrom` | `true` / any | Ingress only to the metrics port |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | | Scheduling |

## Permissions

Everything the agent can do, and why. Two tiers keep powerful rights out of
the cluster-wide binding.

### Tier 1: `heimdall-agent` (ClusterRole, bound cluster-wide)

| Resource | Verbs | Why |
|---|---|---|
| `namespaces` | get, list, watch, create, update, patch, delete | Create and destroy preview namespaces |
| `rolebindings` | get, list, watch, create, update, patch, delete | Bind tier 2 inside each preview namespace |
| `clusterroles` (`heimdall-agent-preview-manager` only) | bind | Grant tier 2 without holding it anywhere |
| pods, services, PVCs, events, service accounts, quotas, limit ranges, deployments, statefulsets, replicasets, jobs, network policies, HTTPRoutes | get, list, watch | Watch every preview with one informer per kind (`rbac.clusterWideRead`) |

Not granted cluster-wide: **secrets, config maps, pod logs, exec**, and any
write except namespaces and role bindings.

Namespace and role-binding writes are confined by two
**ValidatingAdmissionPolicies** that apply only to the agent's service
account:

- `heimdall-agent-namespaces`: the agent may only create, change or delete
  namespaces named `heimdall-*` **and** labelled `heimdall.dev/preview=true`,
  both before and after the change. It cannot adopt an existing namespace by
  labelling it, and cannot touch `kube-system` or its own namespace.
- `heimdall-agent-rolebindings`: role bindings only inside such namespaces,
  and only for `heimdall-agent-preview-manager`.

Set `rbac.clusterWideRead=false` to remove the cluster-wide reads; the agent
then watches preview namespaces one by one with the tier-2 role.

**Policies take effect asynchronously.** The API server starts enforcing a new
admission policy a moment after it is created (observed in the kind e2e:
immediately after `helm install` the policy was not yet active). The agent
therefore checks, before doing any work, that the policy is enforced: a
server-side dry-run creating an unlabelled namespace must be denied by
`heimdall-agent-namespaces`. Until it is, the agent stays not-ready. (The e2e
performs the same check; the agent implements it in P3.)

### Tier 2: `heimdall-agent-preview-manager` (ClusterRole, never bound cluster-wide)

Bound by the agent with a RoleBinding **inside each preview namespace only**.

| Resource | Verbs | Why |
|---|---|---|
| services, config maps, secrets, service accounts, PVCs, resource quotas, limit ranges | full | Rendered guardrails, credentials, seed, database volume |
| deployments, statefulsets, jobs | full | Workloads, dependencies, migrations, seed, smoke tests |
| network policies, HTTPRoutes | full | Isolation and routing |
| pods, pods/log, replicasets | get, list, watch | Status and diagnostics (P4) |
| events | get, list, watch, create, patch | Timeline |

Deliberately absent: `pods/exec`, `pods/attach`, `pods/portforward`, deleting
pods directly (Jobs and Deployments own them), RBAC inside the namespace, and
anything cluster-scoped.

### Own namespace

A Role for leader-election leases and events.

## Security of the agent pod

Non-root (65532), read-only root filesystem, all capabilities dropped,
seccomp `RuntimeDefault`, no privilege escalation, single replica with the
`Recreate` strategy. Its service-account token is mounted because it talks to
the Kubernetes API; preview pods never get one.

## Verifying

```bash
helm lint --strict charts/heimdall-agent
helm template heimdall-agent charts/heimdall-agent --kube-version 1.33.0 | kubeconform -strict -
kubectl auth can-i --list --as system:serviceaccount:heimdall-system:heimdall-agent
```
