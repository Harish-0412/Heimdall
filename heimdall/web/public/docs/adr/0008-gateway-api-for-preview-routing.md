# ADR 0008: Gateway API for preview routing

Status: accepted (P1)

## Context
Every public preview service needs a hostname and TLS. The phase plan fixes
two constraints: **one wildcard certificate per cluster** (per-tenant or
per-preview certificates are noisy and hit issuance rate limits), and
hostnames that are **one DNS label** under the preview domain
(`pr184-shopflow-x7d2.<preview-domain>`) so that single wildcard covers them.

The first plan assumed `Ingress` with ingress-nginx. Two problems:

- **The certificate would enter every preview namespace.** An `Ingress` can
  only reference a TLS Secret in its own namespace, so the wildcard key would
  have to be copied into each preview namespace - the namespaces running
  untrusted pull-request code (ADR 0004).
- **ingress-nginx is retired.** Kubernetes SIG Network ended its maintenance
  in March 2026. Building a new product on it means a forced migration later.

## Decision
Previews are routed with the **Gateway API** (`gateway.networking.k8s.io/v1`,
GA since 2023):

- The platform operator runs **one shared `Gateway` per cluster** (default
  `heimdall-gateway/heimdall`). Its HTTPS listener holds the wildcard
  certificate, which therefore lives in exactly one namespace.
- The listener admits routes only from namespaces labelled
  `heimdall.dev/preview=true` (`allowedRoutes.namespaces.from: Selector`).
- The renderer emits one `HTTPRoute` per public service, attached to that
  Gateway, with the preview hostname. TLS terminates at the Gateway.
- Each preview's default-deny `NetworkPolicy` admits ingress to a public
  service's port only from the gateway's data plane
  (`Platform.IngressPeers`; default: the Gateway's namespace). For data planes
  outside the cluster (AWS ALB in IP mode) the operator sets an `ipBlock` peer.
- Any conformant implementation works: NGINX Gateway Fabric (used in the kind
  end-to-end test), Envoy Gateway, Istio, Cilium, or the AWS Load Balancer
  Controller's Gateway API support on EKS.

Visibility (`private | org | public`) is recorded on each route as the
`heimdall.dev/visibility` annotation; enforcing it (OIDC in front of the
Gateway) is P7's job, and no preview URL is exposed beyond a trusted team
before then.

## Consequences
- The wildcard key never leaves the gateway namespace; a compromised preview
  cannot read it even with a token (and previews have none).
- Cross-namespace attachment is explicit and label-gated, so a namespace that
  is not a preview cannot publish routes on the preview domain.
- Clusters need Gateway API CRDs and an implementation; the agent chart's
  documentation lists this prerequisite. kind tests install both.
- Typed `HTTPRoute` objects come from `sigs.k8s.io/gateway-api`, one more
  dependency of `internal/render` (not of `internal/config`, ADR 0001).
