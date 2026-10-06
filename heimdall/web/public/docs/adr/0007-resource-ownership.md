# ADR 0007: Resource ownership - agent/controller, Helm and Argo CD

Status: accepted (supersedes the Helm-release approach in the first plan's P1/P2)

## Context
The first plan used the Helm SDK to install per-PR releases, server-side apply for
other objects, Helm hooks for migrations, and mentioned Argo CD. Two tools managing
the same objects fight over fields; Helm hooks are hard to retry, inspect and
recover once a release fails.

## Decision: one owner per object

| Tool | Manages | Does not manage |
|---|---|---|
| **Heimdall controller** | Everything inside a preview namespace: Deployments, StatefulSets, Services, Ingresses, Jobs, NetworkPolicies, quotas | The agent/controller itself, cluster add-ons |
| **Helm** | *Distribution* of Heimdall's own components to a cluster (`charts/heimdall-agent`, and our platform for Tier 0) | Preview resources |
| **Argo CD (optional)** | Our platform in Tier 0/2 (API, orchestrator, add-ons: cert-manager, External Secrets, ingress, monitoring) once we have more than a few components | Preview resources; customer clusters |
| **GitHub Actions** | Build, test, scan, push images and the config/fixture bundle | Anything in a cluster |

Argo CD is not needed for the MVP: Terraform plus `helm upgrade` is enough until
platform drift becomes a real problem. Add it deliberately, not by default.

### Rendering and applying previews
- `internal/render` is a pure Go function from `(Config, RenderContext)` to typed
  Kubernetes objects (client-go types), one **stage** at a time. Typed builders
  give compile-time safety and make stages first-class; there is no template
  language to debug. (`heimdall render` prints them as YAML for inspection.)
- The controller applies each stage with **server-side apply**, a single field
  manager, and labels `heimdall.dev/environment` and `heimdall.dev/generation`.
- **Pruning:** SSA does not delete. After a generation succeeds, objects carrying
  an older generation label in that namespace are deleted. Pruning is scoped by
  label selector to the namespace.
- **No Helm hooks.** Every step is a visible Job or a controller condition so it
  can be retried, inspected and timed:

```text
create namespace + guardrails
-> dependencies (Postgres, Redis, RabbitMQ) -> wait ready
-> baseline database: create, run migrations Job, run seed Job, freeze as template
-> clone live database from baseline
-> deploy application (services, workers)
-> smoke-test Jobs
-> ready
```

  Each stage maps to a condition on the CR, an `events` row, and a diagnosis
  scope.

### Resource requests
The first plan's "requests = 25% of limits" rule is withdrawn: small memory
requests let the scheduler pack many memory-hungry pods onto one node, then OOM
kills follow. New rule: CPU may burst (low request); **memory request is close to
the expected working set**.
`requests` is configurable in `heimdall.yaml`; defaults are CPU request 20% of
limit, memory request 50% of limit (floor 64Mi), and per-node memory
over-commitment is capped by the platform. Size presets (`small`, `medium`,
`large`; `large` needs admin approval) give teams a golden path.

## Consequences
- The chart in P1 becomes `charts/heimdall-agent` (install), not an app chart.
- Fewer moving parts and one answer to "who changed this object?".
- We own pruning and ordering logic that Helm would have provided; covered by
  envtest and kind e2e tests.
