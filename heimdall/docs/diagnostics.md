# Diagnostics: what failed and what to do

When a preview fails, Heimdall explains it: the most likely root cause first,
with a stable code, a specific summary, a suggestion and short redacted
evidence. The same diagnosis appears in four places:

| Where | How |
|---|---|
| `heimdall up` / `heimdall reset` | Printed automatically when the operation fails |
| `heimdall diagnose` | On demand, for a live preview or a saved snapshot (`--format text\|json\|markdown`) |
| `PreviewEnvironment` status | `status.diagnoses` (top 5), the `Ready` condition's message and a `Diagnosed` event |
| Pull-request comment (P6) | The markdown renderer (`heimdall diagnose --format markdown` shows it today) |

```text
$ heimdall up heimdall.yaml --state pr-184.json --allow-context kind-dev ...
heimdall: engine.job_failed: Job heimdall-migrate-g2 failed; inspect its logs

MIGRATION_FAILED: Database migration failed (job/heimdall-migrate-g2, stage baseline-db)
  Migration failed: column "owner_id" of relation "catalog_snapshot" contains null values (SQLSTATE 23502)

  What to do: Column `owner_id` was added to `catalog_snapshot` as NOT NULL without a default, but
  `catalog_snapshot` already has rows. Give the column a DEFAULT, or add it as nullable, backfill it,
  and SET NOT NULL in a later migration.

  Evidence:
    | Job BackoffLimitExceeded: Job has reached the specified backoff limit
    | error: column "owner_id" of relation "catalog_snapshot" contains null values
    |   code: '23502',
```

## Codes

Codes are a public contract: dashboards, alerts, PR comments and the control
plane key on them. They are never renamed or reused; new ones are only added.

| Code | Meaning | Typical suggestion |
|---|---|---|
| `CONFIG_INVALID` | `heimdall.yaml` (or the object carrying it) is invalid; a workload needs a secret the tenant has not configured (named) | Fix the config; `heimdall validate` shows the same errors; add the secret or drop it from `secrets:` |
| `POLICY_DENIED` | The config exceeds the tenant policy; an import is not approved; admission rejected the pods | Fit the policy, or ask a platform administrator |
| `STALE_GENERATION` | A newer push superseded this work (warning, not an error) | Look at the newest generation |
| `QUOTA_EXCEEDED` | The preview's ResourceQuota rejected pods | Remove extra pods (manual scale, stuck rollout) or adjust `resources` |
| `NO_CAPACITY` | No node can run a pod: CPU/memory, the preview node pool, or an unprovisioned volume | Lower `resources`, or the platform adds capacity |
| `IMAGE_PULL_FAILED` | An image does not exist, the registry refuses access, or is unreachable | Push the digest CI reports; grant pull access |
| `DB_UNREACHABLE` | An app cannot reach or log in to PostgreSQL, or PostgreSQL is not running | Use the injected `DATABASE_URL`; retry at startup; when PostgreSQL is down, it is named as the cause |
| `MIGRATION_FAILED` | The migration Job failed; the PostgreSQL error is parsed | Specific to the SQLSTATE (below) |
| `SEED_FAILED` | The approved data import failed | Specific to the SQLSTATE |
| `OUT_OF_MEMORY` | A container exceeded its memory limit | Raise `resources.memory`, or fix the leak |
| `CONTAINER_CRASH` | A container keeps exiting; the first error line it printed is quoted | Specific to the exit code and output |
| `HEALTHCHECK_FAILED` | Readiness or liveness checks fail: 4xx, 5xx, nothing listening, too slow | Fix `health.path` or `port:`, or the app |
| `NO_ENDPOINTS` | The preview URL is not served: the Gateway rejected the route or its backend (error), or a service has no ready pods (warning, usually explained by another finding) | Align the base domain with the Gateway listener, let previews attach to it, or fix the workload |
| `SMOKE_TEST_FAILED` | A smoke test failed; curl's error is interpreted | Specific to the HTTP status or connection error |
| `UNCLASSIFIED` | A failure no rule explains yet, or a step that timed out without failing (what was still pending is named); carries the engine's code and message | Inspect `heimdall status` and `heimdall logs`; raise the step timeout for slow steps |

### Migration errors

Migration and import output is parsed for a PostgreSQL error whatever client
printed it (node-postgres, psql, pgx, psycopg, Rails, Prisma, Flyway), by
SQLSTATE notation or PostgreSQL's own message:

| SQLSTATE | Example | Suggestion |
|---|---|---|
| `23502` | column "owner_id" of relation "catalog" contains null values | NOT NULL column added without a default to a table with rows: add a DEFAULT, or add nullable, backfill, then SET NOT NULL |
| `23502` | null value in column "name" of relation "users" violates not-null constraint | Supply a value or a DEFAULT |
| `42701` | column "email" of relation "users" already exists | The migration ran before or overlaps another: `ADD COLUMN IF NOT EXISTS`, check ordering |
| `42P07` | relation "orders" already exists | `CREATE ... IF NOT EXISTS`, check ordering |
| `42601` | syntax error at or near "CRAETE" | Fix the SQL near the token; run it against a local PostgreSQL first |
| `42P01`, `42703` | relation / column does not exist | A migration that creates it must run first |
| `23505`, `23503` | unique / foreign key violation | Deduplicate, or insert referenced rows first |
| `28P01`, `3D000` | wrong password / database | Use the injected `DATABASE_URL` |

Row values PostgreSQL prints in `DETAIL` lines (`Failing row contains (...)`,
`Key (email)=(...)`) are redacted: they are data, not diagnosis.

## Ranking

A failure usually produces several findings; the root cause is ranked first:

1. **Stage order.** A finding in an earlier stage outranks a later one: a
   failed migration (`baseline-db`) outranks the API that is not ready
   (`application`); a crashing PostgreSQL (`dependencies`) outranks the app
   that cannot reach it.
2. **Tier** within a stage: input problems (config, policy) → what prevents
   starting (quota, capacity, images) → data (database, migration, import) →
   runtime (memory, crashes) → readiness (health checks, endpoints) →
   verification (smoke tests).
3. **Dependencies first**, then the code's order, then the subject's name.

Symptoms are dropped when their cause is already explained for the same
workload: no endpoints or failing readiness because its pods crash, run out of
memory or cannot pull. A container killed by its liveness probe is reported as
a health-check problem, not a crash.

Only the current generation counts. Jobs and pods of other generations,
terminating pods and events about objects that no longer exist are ignored, so
history in the namespace never changes the diagnosis (tested: "noise
immunity").

Losing the operation journal's lease is reported first as `UNCLASSIFIED`, with
`engine.lock_lost` and guidance to check cluster/API connectivity before retrying
after the active owner or lease expires. Workload readiness symptoms cannot
explain lost ownership; independent workload findings remain secondary.

Events must also match the current object's UID and current ReplicaSet. During
a rollout, pods from an older workload template do not count against its
replacement. A recovered container's earlier crash or OOM is history. A
successful retry clears the journal's earlier failure for live diagnosis.

## Failing fast

The engine stops waiting for a workload as soon as one of its pods fails in a
way that cannot recover without new input, instead of waiting out the step
timeout (`engine.workload_failed`, terminal for the generation):

| Condition | After |
|---|---|
| Crash loop (backing off or just exited) | 3 restarts |
| Out of memory | 2 kills |
| Image pull failing | 2 minutes (an image pushed late, a registry blip) |
| Invalid image name | immediately |
| Container configuration error (missing secret) | 1 minute |

Scheduling problems are not cut short: an autoscaler may add a node.

## Log hygiene

- **Exact values first.** The collector reads the values Heimdall injected
  (generated credentials, the tenant's secrets, and secrets referenced by pod
  environment variables or volumes) and redacts every nonempty value verbatim,
  including short values, and in standard/URL-safe base64 and URL-encoded forms,
  from every log line, event, message and argument it keeps. If it cannot read
  them, it omits logs and free-form evidence (and says so in `notes`).
- **Patterns are the safety net**: credentials in URLs (the host stays, for
  diagnosis), `password=`-style pairs, bearer tokens, private keys, JWTs and
  well-known token formats (AWS, GitHub, Slack, Stripe, Google, LLM APIs).
- **Capped, without losing the error.** At most 60 lines and 16 KiB per
  container, 20 containers, 300 events; evidence at most 10 lines of 240
  characters; status keeps evidence for the root cause only. A longer log
  keeps its first error lines, then its end (an error printed before a long
  object dump or stack would otherwise fall out of a plain tail). For a
  container that restarted, both its previous and its current output are
  read; when the previous output is gone, the termination message (the tail
  of that output) stands in.
- **Stripped.** Environment values, managed fields and last-applied
  annotations never enter a snapshot; names stay. Init containers and workload
  templates receive the same sanitization. Probe authentication headers and
  unrelated pod payloads are removed.
- Full logs stay in the cluster and are read on demand through the agent with
  the reader's own authorisation (`heimdall logs`); the control plane (P5)
  stores only codes, summaries and short redacted evidence.

Optional LLM summarisation may come later, on top of these rules and after
redaction; the rules remain the source of truth.

## Snapshots

`heimdall diagnose --save-snapshot snapshot.json` writes what the rules looked
at: pods, events, Jobs, workloads, services and endpoint slices, quotas,
HTTPRoutes with the Gateway's verdict on each, log tails, and the failure and
generations the engine recorded, all redacted. `heimdall up --save-snapshot`
keeps the snapshot of a failed operation, including one refused before
anything was created (an import that is not approved). A snapshot can be
attached to a bug report and diagnosed anywhere:

```bash
heimdall diagnose --snapshot snapshot.json --format markdown
```

Every code has a real failure on kind behind it (`make e2e-diagnose`), and the
rules are tested against the snapshots captured from them
(`internal/diagnose/testdata/scenarios`):

| Scenario | How it breaks | Root cause |
|---|---|---|
| `image-pull` | an image digest that was never pushed | `IMAGE_PULL_FAILED` |
| `migration-not-null` | a NOT NULL column added to a table with rows | `MIGRATION_FAILED` (23502) |
| `out-of-memory` | a worker exceeding its 128Mi limit | `OUT_OF_MEMORY` |
| `crash-loop` | a worker command naming a missing file | `CONTAINER_CRASH` |
| `smoke-test` | a smoke test calling a route that does not exist | `SMOKE_TEST_FAILED` |
| `health-check` | `health.path` pointing at a missing endpoint | `HEALTHCHECK_FAILED` |
| `quota` | a service scaled to 40 replicas | `QUOTA_EXCEEDED` |
| `route-rejected` | a base domain outside the Gateway listener's hostname | `NO_ENDPOINTS` |
| `no-capacity` | a preview node pool no node belongs to | `NO_CAPACITY` |
| `database-down` | PostgreSQL scaled to zero under a running preview | `DB_UNREACHABLE` |
| `import-failed` | an approved import reading a table no migration creates | `SEED_FAILED` (42P01) |
| `import-not-approved` | an import whose bytes differ from the approved ones | `POLICY_DENIED` |
| `missing-secret` | a service listing a secret the tenant never configured | `CONFIG_INVALID` |
| `stale-generation` | generation 1 arriving after generation 2 | `STALE_GENERATION` |
| `step-timeout` | a migration that never finishes | `UNCLASSIFIED` (timeout) |

Refresh them with:

```bash
HEIMDALL_E2E_CAPTURE=1 bash test/e2e/diagnose/run.sh
go test ./internal/diagnose -run CapturedScenarios -update   # then review the diff
```

## Adding a rule

1. Reproduce the failure on kind and capture its snapshot (add a scenario to
   `test/e2e/diagnose`), or build a minimal snapshot in `rules_test.go`.
2. Write the rule in `internal/diagnose/rules.go`: specific summary, an
   actionable suggestion, short evidence.
3. If it needs a new code, add it to `codes.go` (never rename one) and to the
   table above.
