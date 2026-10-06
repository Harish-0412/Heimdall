### Preview failed: Database unreachable

**`DB_UNREACHABLE`** in `statefulset/postgres` (stage: `dependencies`)

PostgreSQL is not running (statefulset/postgres is scaled to zero): api cannot reach it

**What to do:** PostgreSQL is down, not the apps that use it. `heimdall up` re-applies the preview and starts it again; if it stops again, its own diagnosis names the cause.

<details><summary>Evidence</summary>

```text
statefulset/postgres is scaled to zero
api: error: terminating connection due to administrator command
```

</details>

<details><summary>Also found (1)</summary>

- **`CONTAINER_CRASH`** api exits with code 1, 1 restarts: error: terminating connection due to administrator command  
  It starts and then exits. The last lines it printed are below; reproduce with `docker run` of the same image and environment, or read more with `heimdall logs --workload api`.

</details>

<sub>`heimdall-pr410-shopflow-e0a4` · generation 1 · run `heimdall diagnose` for details</sub>
