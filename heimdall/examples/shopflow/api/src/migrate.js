// Minimal forward-only migration runner. Heimdall runs it as a Job against the
// baseline database (DATABASE_URL), before the seed and before any service.
import pg from 'pg';
import { required, log } from './env.js';

const migrations = [
  {
    version: 1,
    name: 'create products',
    sql: `CREATE TABLE products (
            id          serial PRIMARY KEY,
            sku         text NOT NULL UNIQUE,
            name        text NOT NULL,
            price_cents integer NOT NULL CHECK (price_cents >= 0),
            created_at  timestamptz NOT NULL DEFAULT now()
          )`,
  },
  {
    version: 2,
    name: 'create orders',
    sql: `CREATE TABLE orders (
            id          serial PRIMARY KEY,
            product_id  integer NOT NULL REFERENCES products (id),
            quantity    integer NOT NULL CHECK (quantity > 0),
            email       text NOT NULL,
            notified_at timestamptz,
            created_at  timestamptz NOT NULL DEFAULT now()
          )`,
  },
];

const client = new pg.Client({ connectionString: required('DATABASE_URL') });
await client.connect();
try {
  await client.query(`CREATE TABLE IF NOT EXISTS schema_migrations (
                        version    integer PRIMARY KEY,
                        applied_at timestamptz NOT NULL DEFAULT now())`);
  const { rows } = await client.query('SELECT version FROM schema_migrations');
  const applied = new Set(rows.map((r) => r.version));
  for (const m of migrations) {
    if (applied.has(m.version)) continue;
    await client.query('BEGIN');
    try {
      await client.query(m.sql);
      await client.query('INSERT INTO schema_migrations (version) VALUES ($1)', [m.version]);
      await client.query('COMMIT');
      log('migration.applied', { version: m.version, name: m.name });
    } catch (err) {
      await client.query('ROLLBACK');
      throw err;
    }
  }
  log('migrations.done', { total: migrations.length });
} finally {
  await client.end();
}
