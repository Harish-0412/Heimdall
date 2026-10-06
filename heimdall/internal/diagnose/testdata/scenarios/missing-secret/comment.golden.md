### Preview failed: Configuration is invalid

**`CONFIG_INVALID`** in `job/heimdall-migrate-g1` (stage: `baseline-db`)

The migration Job cannot start: it needs the secret `PAYMENTS_API_KEY`, which this tenant has not configured

**What to do:** heimdall.yaml lists `PAYMENTS_API_KEY` under `secrets:`, but the platform delivers no such secret to this preview. Add it to the tenant's secrets, or remove it from `secrets:`.

<details><summary>Evidence</summary>

```text
CreateContainerConfigError: secret "heimdall-app-secrets" not found
```

</details>

<sub>`heimdall-pr413-shopflow-002f` · generation 1 · run `heimdall diagnose` for details</sub>
