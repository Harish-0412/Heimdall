# ADR 0003: Strict, versioned, source-located configuration

Status: accepted

## Context
`heimdall.yaml` is written by developers, parsed from untrusted pull requests,
and drives infrastructure. Silent misinterpretation (a typo that disables a
setting) is expensive; unclear errors erode trust.

## Decision
- Unknown fields are errors (`KnownFields(true)`), with "did you mean" hints.
- A required top-level `version`. Additive changes keep the version; breaking
  changes bump it and ship a migration.
- Validation collects *all* problems in one pass. Every finding has a stable
  dotted `code`, a logical `path`, a line/column, and an optional `hint`.
- Warnings never block; `--strict` promotes them for CI.
- Input is size-capped (256 KiB), single-document, and paths must stay inside
  the repository.
- Platform ceilings (`Limits`) are an input to validation, not constants, so the
  API can apply per-tenant quotas.
- Presence enables a dependency; `postgres:` with no value is rejected because
  it decodes to "disabled" - the opposite of intent.

## Consequences
- Slightly less forgiving than permissive YAML; errors are actionable.
- Codes become a public contract: documented, never reused.
- The same loader backs CLI, API and controller.
