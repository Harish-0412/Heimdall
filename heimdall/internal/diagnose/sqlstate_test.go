package diagnose

import (
	"strings"
	"testing"
)

// Real output formats of the clients migrations commonly use.
func TestParseSQLError(t *testing.T) {
	for _, tc := range []struct {
		name, output                         string
		state, table, column, relation, near string
		suggestion                           string
	}{
		{
			name: "node-postgres, the ShopFlow card",
			output: `file:///app/src/migrate.js:46
      throw err;
      ^

error: column "owner_id" of relation "catalog_snapshot" contains null values
    at /app/node_modules/pg/lib/client.js:545:17
    at async file:///app/src/migrate.js:43:7 {
  length: 140,
  severity: 'ERROR',
  code: '23502',
  detail: undefined,
  schema: 'public',
  table: 'catalog_snapshot',
  column: 'owner_id',
  file: 'tablecmds.c',
  line: '6197',
  routine: 'ATRewriteTable'
}

Node.js v24.11.0`,
			state: "23502", table: "catalog_snapshot", column: "owner_id",
			suggestion: "as NOT NULL without a default",
		},
		{
			name:   "psql syntax error",
			output: "psql:migrations/003.sql:1: ERROR:  syntax error at or near \"CRAETE\"\nLINE 1: CRAETE TABLE x ();\n        ^",
			state:  "42601", near: "CRAETE", suggestion: "near `CRAETE`",
		},
		{
			name:   "pgx relation exists",
			output: `migrate: ERROR: relation "users" already exists (SQLSTATE 42P07)`,
			state:  "42P07", relation: "users", suggestion: "IF NOT EXISTS",
		},
		{
			name:   "psycopg duplicate column",
			output: "psycopg2.errors.DuplicateColumn: column \"email\" of relation \"users\" already exists\n",
			state:  "42701", table: "users", column: "email", suggestion: "ADD COLUMN IF NOT EXISTS",
		},
		{
			name: "Rails not-null insert",
			output: "PG::NotNullViolation: ERROR:  null value in column \"name\" of relation \"users\" violates not-null constraint\n" +
				"DETAIL:  Failing row contains (1, null).",
			state: "23502", table: "users", column: "name", suggestion: "`users.name`",
		},
		{
			name: "Prisma",
			output: "Error: P3018\n\nA migration failed to apply.\n\nMigration name: 20261001_owner\n\nDatabase error code: 23502\n\n" +
				"Database error:\nERROR: column \"owner_id\" of relation \"Catalog\" contains null values",
			state: "23502", table: "Catalog", column: "owner_id",
		},
		{
			name:   "Flyway",
			output: "SQL State  : 42P07\nError Code : 0\nMessage    : ERROR: relation \"orders\" already exists",
			state:  "42P07", relation: "orders",
		},
		{
			name:   "undefined table",
			output: `error: relation "orders" does not exist`,
			state:  "42P01", relation: "orders", suggestion: "must run first",
		},
		{
			name:   "column of relation does not exist is not a missing relation",
			output: `ERROR:  column "sku" of relation "products" does not exist`,
			state:  "42703", table: "products", column: "sku",
		},
		{
			name:   "state only",
			output: "migration 7 failed: pq: SQLSTATE 53100",
			state:  "53100",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ParseSQLError(tc.output)
			if e == nil {
				t.Fatal("not recognised")
			}
			if e.State != tc.state || e.Table != tc.table || e.Column != tc.column || e.Relation != tc.relation || e.Near != tc.near {
				t.Errorf("got %+v", *e)
			}
			if e.Name == "" || e.Message == "" {
				t.Errorf("name/message missing: %+v", *e)
			}
			if !strings.Contains(e.Suggestion(), tc.suggestion) {
				t.Errorf("suggestion %q lacks %q", e.Suggestion(), tc.suggestion)
			}
			if !strings.Contains(e.Summary(), "SQLSTATE "+tc.state) {
				t.Errorf("summary %q", e.Summary())
			}
		})
	}
}

func TestParseSQLErrorIgnoresOtherNumbers(t *testing.T) {
	for _, s := range []string{
		"GET /health 200 in 12345ms",
		"listening on :8080, pid 40123",
		"code: 'ECONNREFUSED'",
		"",
	} {
		if e := ParseSQLError(s); e != nil {
			t.Errorf("%q parsed as %+v", s, *e)
		}
	}
}

func TestParseSQLErrorKeepsTheFirstFailure(t *testing.T) {
	for _, output := range []string{
		`ERROR: relation "orders" already exists (SQLSTATE 42P07)` + "\nerror: column \"owner\" of relation \"users\" contains null values\n  code: '23502'",
		`ERROR: syntax error at or near "CRAETE"` + "\nerror: column \"owner\" of relation \"users\" contains null values\n  code: '23502'",
		"code: '12345'\ncode: '42601'\nERROR: syntax error at or near \"CRAETE\"",
	} {
		e := ParseSQLError(output)
		if e == nil || (e.State != "42P07" && e.State != "42601") {
			t.Errorf("first SQL failure lost: %q -> %+v", output, e)
		}
	}
}
