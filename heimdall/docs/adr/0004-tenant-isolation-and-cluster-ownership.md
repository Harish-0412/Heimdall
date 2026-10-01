# ADR 0004: Tenant isolation and cluster ownership

Status: accepted (amends the "customer clusters are post-MVP" position in the first plan)

## Context
Every pull request is treated as hostile input: its Dockerfile, container, config
and seed SQL are attacker-controlled. A Kubernetes namespace separates names,
quotas and network policy, but it shares a kernel, a container runtime and a node
pool with every other namespace. It is **not** a sufficient boundary between
mutually distrusting customers. Planning shared multi-tenant EKS for the MVP while
postponing customer-owned clusters therefore contradicts the threat model.

## Decision
Isolation is defined by tier. The tier decides where preview workloads run.

| Tier | Where previews run | Who it is for | Status |
|---|---|---|---|
| **0 - Trusted single tenant** | One Heimdall-operated cluster, one organisation whose code we trust (ourselves, the demo) | Development, portfolio demo | Built first |
| **1 - Customer cluster (default product)** | Customer's own EKS cluster and AWS account, a node pool they choose, running the **Heimdall agent** | Real customers | Target architecture; the agent is built from P3 |
| **2 - Dedicated managed cluster/account** | Heimdall-operated but one cluster (or account) per tenant | Customers who will not run infrastructure | Later |
| **Not supported** | Shared cluster running code from multiple untrusting tenants | - | Only reconsidered with a hardened sandbox runtime (gVisor/Kata/Firecracker) and a security review |

### Division of responsibility (Tier 1)

- **Heimdall SaaS control plane:** dashboard, API, GitHub App, orchestration,
  audit, quotas, billing metadata, diagnosis *summaries*.
- **Customer account:** cluster, nodes, registry (ECR), preview namespaces,
  databases and fixtures, application logs, preview DNS zone and certificate.
- **Heimdall never stores:** source code, container images, database fixtures, or
  full application logs.

### Consequences for design
- **Outbound-only agent.** The control plane cannot (and must not) reach a
  customer's cluster API. The agent opens an authenticated connection *out* to the
  API, pulls desired state, and pushes status. See ADR 0005.
- **Configuration and fixtures travel as a bundle.** The customer's CI build step
  pushes a *bundle* (normalised config + seed files) to the customer's own
  registry as an OCI artifact, referenced by digest. The agent pulls it with the
  cluster's own credentials. GitHub is read only for metadata (`heimdall.yaml`
  and PR events), never for fixtures.
- **Logs stay in the customer's account.** The control plane stores diagnosis
  codes, short redacted evidence and links; full logs are fetched on demand
  through the agent, authorised per user.
- **Previews receive no cloud credentials by default** (ADR 0007's guardrails;
  see P7). A preview that needs S3 fixtures gets them from the agent, not an IAM role.
- **Same agent everywhere.** Tier 0 runs the exact agent customers run, so the
  demo exercises production code paths. Only the trust level differs.
- **Customer domains.** In Tier 1 the customer owns the preview DNS zone and
  wildcard certificate (cert-manager in their cluster): one certificate per
  cluster, which also avoids certificate-issuance rate limits.
- Tier 1 needs a minimal-RBAC install story (Helm chart), key rotation, and a
  documented list of everything the agent can do in the cluster.

## Alternatives considered
- *Shared multi-tenant cluster with namespaces only* - rejected for anything but Tier 0.
- *Per-tenant vcluster / Kata runtime in a shared cluster* - viable later; adds
  complexity before there is demand.
- *Per-PR dedicated cluster* - strongest isolation, but minutes of start-up time
  and unaffordable per preview.
