### Preview failed: Smoke test failed

**`SMOKE_TEST_FAILED`** in `job/heimdall-smoke-api-health-g1` (stage: `smoke`)

Smoke test "api-health" failed with exit code 22: GET /orders/latest-report on api returned HTTP 404

**What to do:** The path does not exist on api: fix the smoke test's URL, or add the route.

<details><summary>Evidence</summary>

```text
command: curl --fail http://api:8080/orders/latest-report
Job BackoffLimitExceeded: Job has reached the specified backoff limit
curl: (22) The requested URL returned error: 404
```

</details>

<sub>`heimdall-pr405-shopflow-e4cc` · generation 1 · run `heimdall diagnose` for details</sub>
