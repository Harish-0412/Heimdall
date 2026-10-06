### Preview failed: Health check failing

**`HEALTHCHECK_FAILED`** in `deployment/api` (stage: `application`)

api fails its readiness check: GET /healthz on port 8080 returns HTTP 404

**What to do:** `health.path` is /healthz, but the app answers 404 there. Point `health.path` at an endpoint that returns 2xx, or add one.

<details><summary>Evidence</summary>

```text
Unhealthy: Readiness probe failed: HTTP probe failed with statuscode: 404 (x24)
```

</details>

<sub>`heimdall-pr406-shopflow-52ec` · generation 1 · run `heimdall diagnose` for details</sub>
