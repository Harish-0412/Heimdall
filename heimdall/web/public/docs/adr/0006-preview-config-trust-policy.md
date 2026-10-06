# ADR 0006: Preview configuration trust policy

Status: accepted; first slice implemented in P0.1 (`internal/config`: `Policy`, `CompareToBaseline`)

## Context
`heimdall.yaml` lives in the repository, so the pull request under test can edit
it. If Heimdall obeyed whatever the PR says, an author could make a preview public,
raise CPU/memory, extend its lifetime, or reference a sensitive secret.

## Decision
Three layers, each able only to **narrow** the one above it.

```text
Tenant policy         (control plane; hard ceilings and forbidden settings)
   ∩ default-branch heimdall.yaml   (team baseline; reviewed and merged)
      ∩ PR heimdall.yaml            (application-level changes only)
```

1. **Dangerous settings are not expressible in `heimdall.yaml`.** Egress rules,
   node selectors/tolerations, security context, service accounts, IAM, host
   access: they are not in the schema, and strict parsing rejects them as unknown
   fields. They are set by the platform only.
2. **Tenant `Policy`** (`config.Policy`) supplies ceilings the file is validated
   against: resource and quota `Limits`, `MaxVisibility`, `AllowedSecrets`,
   `AllowedRegistries`. Violations are `policy.*` errors.
3. **Baseline comparison** (`config.CompareToBaseline`): the PR file may not
   loosen the default-branch file. Violations are `trust.*` errors.

### What a PR may and may not change

| PR may change (application level) | PR may not exceed the default branch's value |
|---|---|
| build context, Dockerfile, build args | `preview.visibility` (`trust.visibility`) |
| image reference *within allowed registries* | `preview.ttl` (`trust.ttl`) |
| ports, health paths, commands | secrets referenced: only names already used by the default branch (`trust.secret`) |
| non-sensitive env vars | per-workload CPU/memory; new workloads are capped at the defaults (`trust.resources`) |
| migration and smoke-test commands | database storage (`trust.resources`) |
| seed file path, new services/workers/dependencies *within quotas* | |

Anything in the right-hand column requires a pull request to the **default
branch**, reviewed under branch protection; previews then accept it.

### Missing baseline
First onboarding has no default-branch file. Then the PR file is validated against
the tenant policy only and the first deployment requires an explicit maintainer
approval (same mechanism as ADR on fork PRs: P6). `heimdall init` output is meant
to be merged to the default branch first.

### Who evaluates
The control plane evaluates trust when handling a webhook (it fetches both files via
the GitHub App) and again in the controller's admission webhook. The agent never
trusts a CR it did not receive from the control plane.

## Consequences
- Policy is an input to `Load`, not a constant, so quotas can differ per plan.
- Error codes `policy.*` and `trust.*` are public contract (documented in the
  config reference).
- Slightly more friction for legitimate increases; the error message tells the
  author exactly what to do.
