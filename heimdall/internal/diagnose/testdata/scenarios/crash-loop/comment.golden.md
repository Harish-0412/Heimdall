### Preview failed: Container crashes

**`CONTAINER_CRASH`** in `deployment/notifications` (stage: `application`)

notifications exits with code 1, 3 restarts: Error: Cannot find module '/app/src/notifier.js'

**What to do:** It starts and then exits. The last lines it printed are below; reproduce with `docker run` of the same image and environment, or read more with `heimdall logs --workload notifications`.

<details><summary>Evidence</summary>

```text
container notifications: Error, exit code 1
Error: Cannot find module '/app/src/notifier.js'
  code: 'MODULE_NOT_FOUND',
  requireStack: []
}
Node.js v24.21.0
```

</details>

<sub>`heimdall-pr404-shopflow-856d` · generation 1 · run `heimdall diagnose` for details</sub>
