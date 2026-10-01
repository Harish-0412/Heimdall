# Heimdall

Secure, disposable, full-stack environments for every pull request.

Open PR `#184` and Heimdall builds it, deploys the whole stack (frontend, API,
PostgreSQL, Redis, RabbitMQ, workers) into an isolated Kubernetes namespace,
seeds it, runs smoke tests and comments the URL on the PR. Close the PR and
everything is destroyed.

> Status: **P0, P0.1 and P1 complete** - `heimdall.yaml` schema and
> validation, the config trust policy, and rendering: a validated config
> becomes typed Kubernetes objects, stage by stage, with secure defaults in
> every pod. See [docs/phases.md](docs/phases.md) for the roadmap.

## Quick start

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
step: 09-smoke-run
```

| Command | Purpose |
|---|---|
| `heimdall validate [--strict] [--baseline file] [--format json] [file]` | Check a config; `--baseline` fails if a PR loosens the default branch's config |
| `heimdall render [flags] [file]` | Print the Kubernetes objects of a preview, by stage; `--out-dir` writes one file per step |
| `heimdall schema` | JSON Schema for editors (also at [`schema/heimdall.schema.json`](schema/heimdall.schema.json)) |

Exit codes: `0` success, `1` invalid config or render input, `2` usage / IO error.

## Repository layout

```text
cmd/heimdall/          CLI entrypoint (thin)
internal/cli/          command implementations (testable: Run(args, out, err) int)
internal/config/       heimdall.yaml schema, strict loader, validator, policy, JSON Schema
internal/render/       Config + Context -> staged, typed Kubernetes objects
internal/version/      build metadata injected via -ldflags
charts/heimdall-agent/ Helm chart that installs the agent (minimal, documented RBAC)
schema/                generated JSON Schema for heimdall.yaml
examples/shopflow/     sample config and the runnable ShopFlow demo app
test/e2e/kind/         end-to-end test on kind (the P1 exit criterion)
docs/                  design, phases, rendering, ADRs, config reference
```

Later phases add `cmd/{api,orchestrator,agent,webhook}`, `internal/engine`,
`internal/controller`, `internal/store`, `deploy/`.

## Development

```bash
make test           # go test -race ./...
make lint           # golangci-lint
make golden         # regenerate render golden files (review the diff)
make schema         # regenerate schema/heimdall.schema.json
make kubeconform    # validate every rendered object and the chart (needs kubeconform, helm)
make helm-lint
make fuzz
make e2e-kind       # ShopFlow on kind end to end (needs Docker, kind, kubectl)
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
- [heimdall.yaml reference](docs/config-reference.md)
- [Rendering: what Heimdall creates in the cluster](docs/rendering.md)
- [Agent chart and its permissions](charts/heimdall-agent/README.md)
- [Architecture decision records](docs/adr)
