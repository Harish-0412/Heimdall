### Preview failed: Data import failed

**`SEED_FAILED`** in `job/heimdall-seed-g1` (stage: `baseline-db`)

Data import failed: relation "legacy\_products" does not exist (SQLSTATE 42P01)

**What to do:** Relation `legacy_products` does not exist yet: a migration that creates it must run first. Check the migrations' order.

<details><summary>Evidence</summary>

```text
Job BackoffLimitExceeded: Job has reached the specified backoff limit
psql:/seed/seed.sql:2: ERROR:  relation "legacy_products" does not exist
LINE 1: SELECT count(*) FROM legacy_products;
```

</details>

<sub>`heimdall-pr411-shopflow-4bb2` · generation 1 · run `heimdall diagnose` for details</sub>
