### Preview failed: Out of memory

**`OUT_OF_MEMORY`** in `deployment/notifications` (stage: `application`)

notifications was killed for exceeding its 128Mi memory limit (2 restarts)

**What to do:** Raise `resources.memory` for notifications in heimdall.yaml, or find what makes it use more memory than in production (a cache without a bound, loading a whole table).

<details><summary>Evidence</summary>

```text
container notifications: OOMKilled, exit code 137
```

</details>

<sub>`heimdall-pr403-shopflow-01d4` · generation 1 · run `heimdall diagnose` for details</sub>
