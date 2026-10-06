# ADR 0001: Go, single module monorepo

Status: accepted

## Context
Heimdall's core is a Kubernetes operator plus a control-plane API. The
Kubernetes ecosystem (client-go, controller-runtime, the Helm SDK) is Go-native.
The CLI, API, orchestrator, controller and webhook must agree on config
validation and the environment state machine.

## Decision
One Go module, many `cmd/*` binaries, shared `internal/*` packages. Binaries
stay thin; logic lives in packages that tests can call directly.

## Consequences
- A config file that validates in the CLI validates identically in the API and
  the controller (same `internal/config.Load`).
- Refactors across components are atomic.
- All binaries share one dependency graph, so heavy dependencies (for example
  `k8s.io/*`) must not leak into packages the Lambda webhook imports. `internal/config`
  deliberately avoids them.
- `internal` prevents accidental external use until we choose to publish an API.
