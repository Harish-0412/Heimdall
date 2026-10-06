# Heimdall

Secure, disposable, full-stack environments for every pull request.

The GitHub workflow is: open PR `#184` and Heimdall builds it, deploys
the whole stack (frontend, API, PostgreSQL, Redis, RabbitMQ, workers) into an
isolated Kubernetes namespace, initialises its schema, runs smoke tests and
comments the URL on the PR. Close the PR and everything is destroyed.

> Status: **P0–P5 complete; P6 implemented with live acceptance pending** —
> configuration, trust policy, secure rendering, lifecycle engine, agent,
> diagnostics, persistent control plane and GitHub integration.
> Supply built application images by digest and deploy through the CLI or a
> `PreviewEnvironment`; `up`, `reset`, `down`, `status`, `logs` and `diagnose`
> operate on real Kubernetes resources. The outbound agent also accepts API
> intent. Automatic GitHub delivery requires installing the new own App,
> configuring the trusted workflow and customer registry, and completing the
> real repository acceptance test. Defaults contain no synthetic application data.
> See [the P5/P6 completion review](docs/phase5-6-completion-review.md),
> [control-plane setup](docs/control-plane.md), [GitHub App setup](docs/github-app-setup.md)
> and the [roadmap](docs/phases.md).

## Quick start

The P11 dashboard is available locally in `web/`: run `npm install` and
`npm run dev`, then open `/dashboard`. It includes a clearly labelled sample
workspace and an optional live control-plane connection. See the complete
[component design and implementation plan](docs/dashboard-design.md),
[first-preview guide](docs/dashboard-quickstart.md) and
[launch demonstration](docs/launch-demo.md). P11 has started; live launch gates
remain pending.

Requires Go 1.26+.

```bash
make build
./bin/heimdall validate examples/shopflow/heimdall.yaml
./bin/heimdall render --placeholder-images --list examples/shopflow/heimdall.yaml
```

```text
$ heimdall validate broken.yaml
broken.yaml:12:9: error[port.invalid]: port 70000 is out of range (1-65535)
broken.yaml:15:5: error[dependsOn.cycle]: dependency cycle: a -> b -> a
    hint: Break the loop by removing one of these edges.
broken.yaml: 2 errors, 0 warnings

$ heimdall render --placeholder-images --list examples/shopflow/heimdall.yaml
namespace: heimdall-pr1-shopflow-6ccf
url: api http://pr1-shopflow-api-6ccf.localtest.me
url: web http://pr1-shopflow-6ccf.localtest.me (primary)
step: 01-guardrails-setup
step: 02-dependencies-start
step: 03-baseline-db-prepare
...
step: 08-smoke-run
```

| Command | Purpose |
|---|---|
| `heimdall validate [--strict] [--baseline file] [--format json] [file]` | Check a config; `--baseline` fails if a PR loosens the default branch's config |
| `heimdall render [flags] [file]` | Print the Kubernetes objects of a preview, by stage; `--out-dir` writes one file per step |
| `heimdall schema` | JSON Schema for editors (also at [`schema/heimdall.schema.json`](schema/heimdall.schema.json)) |
| `heimdall up [flags] [file]` | Reconcile an explicit generation; requires actual images and an allowed kube-context; a failure prints its diagnosis (`--save-snapshot` keeps it) |
| `heimdall down/reset/status/logs --state file --allow-context name` | Operate on the saved preview intent; reset also requires `--nonce` |
| `heimdall force-cleanup --state file --allow-context name --reason text --evidence file` | Administrator-only recovery of a stuck terminating namespace |
| `heimdall diagnose --state file --allow-context name [--format text\|json\|markdown]` | Explain why a preview failed and what to do ([diagnostics](docs/diagnostics.md)); `--snapshot` diagnoses a saved snapshot |
| `heimdall manifest [flags] [file]` | Print a `PreviewEnvironment` for the in-cluster agent (`\| kubectl apply -f -`) |
| `heimdall init --workflow owner/repo/.github/workflows/heimdall-preview.yml@SHA [--out-dir dir]` | Prepare reviewed, immutable workflow onboarding files |
| `heimdall bundle pack/push [flags]` | Transport declared configuration and attested fixture as a reproducible digest-pinned OCI artifact |

Exit codes: `0` success, `1` invalid config or render input, `2` usage / IO error.

## Repository layout

```text
cmd/heimdall/          CLI entrypoint (thin)
cmd/agent/             in-cluster agent entrypoint (build/agent/Dockerfile)
cmd/api/               control-plane REST/SSE and outbound agent endpoints
cmd/orchestrator/      FIFO consumer and durable GitHub notification worker
cmd/webhook/           signed webhook Lambda ingress
cmd/control/           operator migration, bootstrap and registration utility
internal/cli/          command implementations (testable: Run(args, out, err) int)
internal/config/       heimdall.yaml schema, strict loader, validator, policy, JSON Schema
internal/render/       Config + Context -> staged, typed Kubernetes objects
internal/engine/       Reconciliation, operation journal, watches, data lifecycle and pruning
internal/api/v1alpha1/ PreviewEnvironment CRD types (charts/heimdall-agent/crds is generated)
internal/controller/   PreviewEnvironment reconciler: async operations, fencing, status
internal/source/       desired-state sources (API, cluster, configmap, file) and the syncer
internal/controlapi/   generated HTTP contract, authorization and ephemeral logs
internal/controlclient/ durable cluster credential rotation and outbound requests
internal/store/        restricted-role PostgreSQL intent, history, inbox and outbox
internal/githubapp/    own App credentials, canonical REST reads and OIDC verification
internal/orchestrator/ canonical PR lifecycle, trust checks and authorized commands
internal/bundle/       contained OCI configuration and fixture artifacts
internal/sweeper/      fail-safe orphan removal
internal/webhook/      admission webhook and its self-managed certificates
internal/agent/        agent configuration, wiring, access and admission-policy guard
internal/diagnose/     failure diagnosis: snapshot -> ranked codes, suggestions, evidence
internal/redact/       secret redaction for logs, evidence and comments
internal/version/      build metadata injected via -ldflags
charts/heimdall-agent/ Helm chart that installs the agent (minimal, documented RBAC)
schema/                generated JSON Schema for heimdall.yaml
examples/shopflow/     sample config and the runnable ShopFlow demo app
test/e2e/              kind end-to-end tests: kind (P1), engine (P2), agent (P3), diagnose (P4), control (P5)
deploy/local/          persistent PostgreSQL, ephemeral Redis and optional own App runtime
infra/                 webhook FIFO/DLQ ingress and scoped registry IAM templates
docs/                  design, phases, rendering, engine, agent, diagnostics, ADRs
```

GitHub Actions workflows are at the Git root, `../.github/workflows/`.

## Development

```bash
make test           # go test -race ./...
make lint           # golangci-lint
make golden         # regenerate render golden files (review the diff)
make diagnostics-golden # regenerate all 15 diagnostics output fixtures (review the diff)
make schema         # regenerate schema/heimdall.schema.json
make kubeconform    # validate every rendered object and the chart (needs kubeconform, helm)
make helm-lint
make fuzz
make e2e-kind       # ShopFlow on kind end to end (needs Docker, kind, kubectl)
make e2e-engine     # real CLI lifecycle, two previews, reset, storage change and teardown
make envtest        # controller, CRD and syncer suites against a real API server
make generate       # PreviewEnvironment deep copies and CRD
make build-agent    # build the agent binary
make docker-agent   # the agent image
make e2e-agent      # P3 exit criteria: the agent on kind, agent kills, sweeper
make e2e-diagnose   # P4 exit criteria: all 15 failure scenarios, check each diagnosis
make build-control  # API, operator, orchestrator and Lambda binaries
make test-integration # restricted PostgreSQL, ephemeral Redis and real LocalStack FIFO
make e2e-control    # P5 API-only lifecycle, two-replica agent and recovery on kind
make generate-api generate-sql # regenerate HTTP and database contracts
```

## Landing page

The React + TypeScript landing page lives in [`web/`](web/README.md). It includes
the project overview, security model, working CLI setup, roadmap and an interactive
deployment walkthrough.

```bash
cd web
npm install
npm run dev
```

Build a static production bundle with `npm run build`. See the web README for
browser checks and hosting notes.

## Project guides

- [Backend design](docs/backend-design.md)
- [Phases and implementation plan](docs/phases.md)
- [Control plane and agent setup](docs/control-plane.md)
- [Own GitHub App setup and live acceptance](docs/github-app-setup.md)
- [P5/P6 completion review](docs/phase5-6-completion-review.md)
- [heimdall.yaml reference](docs/config-reference.md)
- [Rendering: what Heimdall creates in the cluster](docs/rendering.md)
- [Local engine and data lifecycle](docs/engine.md)
- [The agent: PreviewEnvironment controller](docs/agent.md)
- [Diagnostics: what failed and what to do](docs/diagnostics.md)
- [Agent chart and its permissions](charts/heimdall-agent/README.md)
- [Architecture decision records](docs/adr)
