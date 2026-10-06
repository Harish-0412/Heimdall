### Preview failed: Failed

**`UNCLASSIFIED`** in `job/heimdall-migrate-g1` (stage: `baseline-db`)

step baseline-db/migrate did not finish within the step timeout: job/heimdall-migrate-g1 was still running

**What to do:** It was still running, not failing: a long data migration or import, or one waiting on a lock or for input. Make it faster or non-interactive, or raise the step timeout (`--timeout`, operations.stepTimeout).

<details><summary>Evidence</summary>

```text
job/heimdall-migrate-g1 was still running
```

</details>

<sub>`heimdall-pr415-shopflow-e100` · generation 1 · run `heimdall diagnose` for details</sub>
