# heimdall.yaml reference (schema version 1)

Loaded strictly: unknown fields are errors. Durations accept Go syntax plus
days (`90m`, `48h`, `2d`). The authoritative definitions are the Go types in
[`internal/config/config.go`](../internal/config/config.go).

**Editor support:** a JSON Schema generated from those types is committed at
[`schema/heimdall.schema.json`](../schema/heimdall.schema.json) (also
`heimdall schema`). Add this first line for completion and inline errors in
VS Code, JetBrains and other YAML-language-server editors:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/heimdall-dev/heimdall/main/schema/heimdall.schema.json
```

The schema catches typos, wrong types and simple value errors; cross-field
rules (one primary, cycles, policy, quotas) are checked by `heimdall validate`
only. Every file `validate` accepts also matches the schema.

What a config turns into in the cluster is described in
[rendering.md](rendering.md).

```yaml
version: 1                     # required

services:                      # at least one
  <name>:                      # lowercase letters/digits/'-', <= 40 chars
    build: {context: ., dockerfile: Dockerfile, args: {K: v}}   # xor image
    image: nginx:1.27
    port: 8080                 # required
    public: true               # expose via preview URL
    primary: true              # owns the bare URL; required if >1 public
    env: {KEY: value}          # no HEIMDALL_ prefix, no injected names
    secrets: [STRIPE_TEST_KEY] # UPPER_SNAKE_CASE, stored in Heimdall
    health: {path: /health, initialDelay: 5s}
    resources: {size: medium}  # or explicit limits, optionally with requests:
    # resources: {cpu: 500m, memory: 512Mi, requests: {cpu: 100m, memory: 256Mi}}
    dependsOn: [postgres, other-service]

workers:
  <name>:
    build | image
    command: npm run worker    # required, via sh -c
    replicas: 1                # 1-5
    env / secrets / resources / dependsOn

dependencies:                  # presence enables; use {} for defaults
  postgres: {version: "16", storage: 1Gi, seed: fixtures/dev.sql}
  redis: {version: "7"}
  rabbitmq: {version: "3.13"}

migrations: {service: api, command: npm run migrate, timeout: 5m}

smokeTests:
  - {name: api-health, command: "curl -f http://api:8080/health", timeout: 2m}

preview: {ttl: 48h, sleepAfter: 4h, visibility: private}   # private|org|public
```

## Resources

`resources` sizes one container. Either pick a **size preset** or give
explicit **limits** (`cpu`, `memory`) with optional **requests**:

| Size | CPU limit | Memory limit | Notes |
|---|---|---|---|
| `small` | 250m | 256Mi | worker default |
| `medium` | 500m | 512Mi | service default |
| `large` | 1 | 2Gi | needs `AllowLargeSize` in the tenant policy |

Requests default to **20% of the CPU limit** (at least 10m; CPU may burst) and
**50% of the memory limit** (at least 64Mi; memory requests stay close to the
working set so nodes are not over-packed). A request may not exceed its limit,
and a memory request may not be below the policy's minimum share of the
memory limit (50% by default), so memory over-commitment is at most 2x.

## Injected environment

Heimdall sets `PORT` (services), `DATABASE_URL`, `REDIS_URL`, `AMQP_URL` (when
the dependency is enabled), `HEIMDALL_PR`, `HEIMDALL_SHA`,
`HEIMDALL_PUBLIC_URL` and `HEIMDALL_PUBLIC_URL_<SERVICE>`. `env` and `secrets`
may not redefine them. Details: [rendering.md](rendering.md#environment-variables).

## Defaults

| Field | Default |
|---|---|
| service resources | `medium`: `500m` CPU / `512Mi`; requests `100m` / `256Mi` |
| worker resources | `small`: `250m` CPU / `256Mi`; requests `50m` / `128Mi`; 1 replica |
| build.context / dockerfile | `.` / `Dockerfile` (relative to context) |
| postgres version / storage | `16` / `1Gi` |
| redis / rabbitmq version | `7` / `3.13` |
| migrations.timeout / smoke timeout | `5m` / `2m` |
| preview.ttl / visibility | `48h` / `private` |
| primary service | the only public service, if exactly one |

## Default limits (per environment)

8 services, 8 workers, 2 CPU / 4Gi per container, 6 CPU / 12Gi total
(including 500m/512Mi for Postgres, 100m/128Mi Redis, 250m/512Mi RabbitMQ),
5Gi database, TTL 1h-7d. The API applies tenant-specific limits.

## Trust model

A PR can edit `heimdall.yaml`, so risky settings are bounded from outside the PR
([ADR 0006](adr/0006-preview-config-trust-policy.md)):

- **Not expressible at all:** egress rules, node selectors, security context,
  service accounts, IAM. They are not in the schema (unknown fields are errors).
- **Tenant policy** (`Policy`): `MaxVisibility`, `AllowedSecrets`,
  `AllowedRegistries`, `AllowLargeSize`, resource/quota `Limits` (including
  `MinMemoryRequestPercent`). Violations are `policy.*`.
- **Baseline comparison:** `heimdall validate --baseline <default-branch file> <pr file>`.
  A PR may not loosen visibility, ttl, secrets, per-workload resources or database
  storage relative to the default branch (`trust.*`). New workloads are capped at
  the defaults. Change those in a PR to the default branch first.

## Diagnostic codes

Errors block; warnings do not (unless `--strict`). Codes are stable.

| Code | Severity | Meaning |
|---|---|---|
| `file.empty` / `file.too_large` / `file.multiple_documents` / `file.unreadable` | error | Input problems |
| `yaml.syntax` / `yaml.null` | error | Malformed YAML; `postgres:` with no value |
| `field.unknown` / `type.mismatch` / `value.invalid` | error | Unknown field (with suggestion), wrong type, unparsable value |
| `version.missing` / `version.unsupported` | error | Schema version |
| `name.invalid` / `name.reserved` / `name.duplicate` | error | Naming rules (`heimdall-*` names are reserved for platform objects) |
| `services.empty` / `services.too_many` / `workers.too_many` | error | Counts |
| `source.missing` / `source.conflict` / `image.invalid` / `path.invalid` / `build.arg.invalid` | error | Build / image / path rules |
| `port.missing` / `port.invalid` / `health.path.invalid` / `health.delay.out_of_range` | error | Networking |
| `env.name.invalid` / `env.reserved` / `secret.invalid` / `secret.duplicate` / `secret.conflict` | error | Env and secrets (`env.reserved` also covers injected names) |
| `resources.invalid` / `resources.too_large` / `quota.cpu` / `quota.memory` | error | Sizing and quota |
| `resources.size.invalid` / `resources.size.conflict` | error | Unknown preset; preset combined with explicit values |
| `resources.requests.exceeds_limit` / `resources.requests.too_low` | error | Request above its limit; memory over-commit beyond policy |
| `worker.command.missing` / `worker.replicas.out_of_range` | error | Workers |
| `postgres.version.unsupported` / `redis.version.unsupported` / `rabbitmq.version.unsupported` / `seed.extension` | error | Dependencies |
| `migrations.requires_postgres` / `migrations.service.missing` / `migrations.service.unknown` / `migrations.command.missing` / `migrations.timeout.out_of_range` | error | Migrations |
| `smoke.*` | error | Smoke test rules |
| `dependsOn.unknown` / `.self` / `.worker` / `.cycle` | error | Dependency graph |
| `service.primary.ambiguous` / `.multiple` / `.not_public` | error | Public URL ownership |
| `preview.ttl.out_of_range` / `preview.sleepAfter.out_of_range` / `preview.visibility.invalid` | error | Lifecycle |
| `policy.visibility_denied` / `policy.secret_denied` / `policy.image_denied` / `policy.size_denied` | error | Violates tenant policy |
| `trust.visibility` / `trust.ttl` / `trust.secret` / `trust.resources` | error | PR loosens the default branch's config (`--baseline`) |
| `health.missing` | warning | TCP probe will be used |
| `service.public.none` | warning | No preview URL |
| `env.secret_literal` | warning | Secret-looking env var with a literal value |
| `dependsOn.duplicate` | warning | Repeated entry |
| `preview.visibility.public` | warning | Anyone with the URL can open it |
