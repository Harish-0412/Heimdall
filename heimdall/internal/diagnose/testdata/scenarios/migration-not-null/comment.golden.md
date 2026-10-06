### Preview failed: Database migration failed

**`MIGRATION_FAILED`** in `job/heimdall-migrate-g1` (stage: `baseline-db`)

Migration failed: column "owner\_id" of relation "catalog\_snapshot" contains null values (SQLSTATE 23502)

**What to do:** Column `owner_id` was added to `catalog_snapshot` as NOT NULL without a default, but `catalog_snapshot` already has rows. Give the column a DEFAULT, or add it as nullable, backfill it, and SET NOT NULL in a later migration.

<details><summary>Evidence</summary>

```text
Job BackoffLimitExceeded: Job has reached the specified backoff limit
error: column "owner_id" of relation "catalog_snapshot" contains null values
  code: '23502',
  table: 'catalog_snapshot',
  column: 'owner_id',
```

</details>

<sub>`heimdall-pr402-shopflow-a65a` · generation 1 · run `heimdall diagnose` for details</sub>
