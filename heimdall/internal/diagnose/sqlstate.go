package diagnose

import (
	"fmt"
	"regexp"
	"strings"
)

// SQLError is a PostgreSQL error recognised in a migration's or import's
// output, whatever printed it: node-postgres, psql, pgx, psycopg, Rails,
// Prisma, Flyway.
type SQLError struct {
	// State is the SQLSTATE ("23502"), when it could be determined.
	State string `json:"state,omitempty"`
	// Name is the condition name ("not_null_violation").
	Name string `json:"name,omitempty"`
	// Message is PostgreSQL's primary message.
	Message  string `json:"message"`
	Table    string `json:"table,omitempty"`
	Column   string `json:"column,omitempty"`
	Relation string `json:"relation,omitempty"`
	// Near is the token a syntax error points at.
	Near string `json:"near,omitempty"`
	// Constraint is the violated constraint, when named.
	Constraint string `json:"constraint,omitempty"`
}

// sqlStates names the states Heimdall explains (PostgreSQL appendix A).
var sqlStates = map[string]string{
	"23502": "not_null_violation", "23503": "foreign_key_violation", "23505": "unique_violation",
	"23514": "check_violation", "42601": "syntax_error", "42701": "duplicate_column", "42P07": "duplicate_table",
	"42P01": "undefined_table", "42703": "undefined_column", "42804": "datatype_mismatch", "42883": "undefined_function",
	"42501": "insufficient_privilege", "22P02": "invalid_text_representation", "25P02": "in_failed_sql_transaction",
	"57014": "query_canceled", "53100": "disk_full", "28P01": "invalid_password", "3D000": "invalid_catalog_name",
}

// Explicit SQLSTATE notations, by client.
var statePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bSQLSTATE\s*[\[:=(]?\s*([0-9A-Z]{5})\b`),        // pgx, psql \set VERBOSITY, PDO
	regexp.MustCompile(`\bcode:\s*'([0-9A-Z]{5})'`),                      // node-postgres (util.inspect)
	regexp.MustCompile(`"code"\s*:\s*"([0-9A-Z]{5})"`),                   // JSON-logged errors
	regexp.MustCompile(`(?i)\bSQL State\s*:\s*([0-9A-Z]{5})\b`),          // Flyway
	regexp.MustCompile(`(?i)\bDatabase error code:\s*([0-9A-Z]{5})\b`),   // Prisma
	regexp.MustCompile(`(?i)\bsql_?state\s*[=:]\s*['"]?([0-9A-Z]{5})\b`), // JDBC, logs
	regexp.MustCompile(`\bpgcode\s*[=:]\s*['"]?([0-9A-Z]{5})\b`),         // psycopg
}

// PostgreSQL's English primary messages for the states explained here.
var messagePatterns = []struct {
	state string
	re    *regexp.Regexp
}{
	{"23502", regexp.MustCompile(`column "([^"]+)" of relation "([^"]+)" contains null values`)},
	{"23502", regexp.MustCompile(`null value in column "([^"]+)"(?: of relation "([^"]+)")? violates not-null constraint`)},
	{"42701", regexp.MustCompile(`column "([^"]+)" of relation "([^"]+)" already exists`)},
	{"42P07", regexp.MustCompile(`relation "([^"]+)" already exists`)},
	{"42601", regexp.MustCompile(`syntax error at or near "([^"]*)"`)},
	{"42601", regexp.MustCompile(`syntax error at end of input`)},
	{"42P01", regexp.MustCompile(`relation "([^"]+)" does not exist`)},
	{"42703", regexp.MustCompile(`column "([^"]+)"(?: of relation "([^"]+)")? does not exist`)},
	{"23505", regexp.MustCompile(`duplicate key value violates unique constraint "([^"]+)"`)},
	{"23503", regexp.MustCompile(`(?:insert or update on table "([^"]+)" )?violates foreign key constraint "([^"]+)"`)},
	{"28P01", regexp.MustCompile(`password authentication failed for user "([^"]+)"`)},
	{"3D000", regexp.MustCompile(`database "([^"]+)" does not exist`)},
}

// node-postgres prints the error's fields.
var (
	fieldTable      = regexp.MustCompile(`\btable:\s*'([^']+)'`)
	fieldColumn     = regexp.MustCompile(`\bcolumn:\s*'([^']+)'`)
	fieldConstraint = regexp.MustCompile(`\bconstraint:\s*'([^']+)'`)
)

// ParseSQLError finds the first PostgreSQL error in output, or nil.
func ParseSQLError(output string) *SQLError {
	var e SQLError
	stateLine := ""
	stateAt := -1
	for _, p := range statePatterns {
		for _, loc := range p.FindAllStringSubmatchIndex(output, -1) {
			if looksLikeState(output[loc[2]:loc[3]]) && (stateAt < 0 || loc[0] < stateAt) {
				e.State = output[loc[2]:loc[3]]
				stateLine = lineAt(output, loc[0])
				stateAt = loc[0]
			}
		}
	}
	explicit := e.State
	first := -1
	for _, mp := range messagePatterns {
		loc := mp.re.FindStringSubmatchIndex(output)
		if loc == nil || (first >= 0 && loc[0] >= first) {
			continue
		}
		if explicit != "" && mp.state != explicit && loc[0] >= stateAt {
			continue
		}
		first = loc[0]
		m := submatches(output, loc)
		e.Message = output[loc[0]:loc[1]]
		if explicit == "" || loc[0] < stateAt {
			e.State = mp.state
		}
		e.Table, e.Column, e.Relation, e.Near, e.Constraint = "", "", "", "", ""
		switch {
		case strings.Contains(e.Message, "contains null values"), strings.Contains(e.Message, "already exists") && strings.HasPrefix(e.Message, "column"):
			e.Column, e.Table = m[1], m[2]
		case strings.HasPrefix(e.Message, "null value in column"), strings.HasPrefix(e.Message, "column") && strings.HasSuffix(e.Message, "does not exist"):
			e.Column, e.Table = m[1], m[2]
		case strings.HasPrefix(e.Message, "relation"):
			e.Relation = m[1]
		case strings.HasPrefix(e.Message, "syntax error at or near"):
			e.Near = m[1]
		case strings.HasPrefix(e.Message, "duplicate key"):
			e.Constraint = m[1]
		case strings.Contains(e.Message, "foreign key"):
			e.Table, e.Constraint = m[1], m[2]
		}
	}
	if e.State == "" {
		return nil
	}
	if e.Message == "" {
		// A state without a recognised message: take the line that carries
		// PostgreSQL's "ERROR:" or the client's error text.
		e.Message = errorLine(output)
		if e.Message == "" {
			e.Message = stateLine
		}
	}
	if m := fieldTable.FindStringSubmatch(output); m != nil && e.Table == "" && e.Relation == "" {
		e.Table = m[1]
	}
	if m := fieldColumn.FindStringSubmatch(output); m != nil && e.Column == "" {
		e.Column = m[1]
	}
	if m := fieldConstraint.FindStringSubmatch(output); m != nil && e.Constraint == "" {
		e.Constraint = m[1]
	}
	e.Name = sqlStates[e.State]
	return &e
}

func submatches(s string, loc []int) []string {
	out := make([]string, len(loc)/2)
	for i := range out {
		if loc[2*i] >= 0 {
			out[i] = s[loc[2*i]:loc[2*i+1]]
		}
	}
	return out
}

// looksLikeState accepts a SQLSTATE: a known class (two characters) and three
// more. It rejects HTTP codes and the like that happen to have five digits.
func looksLikeState(s string) bool {
	if _, ok := sqlStates[s]; ok {
		return true
	}
	switch s[:2] {
	case "08", "22", "23", "25", "28", "3D", "3F", "40", "42", "53", "54", "55", "57", "58", "XX":
		return true
	}
	return false
}

// lineAt returns the trimmed line containing offset i.
func lineAt(s string, i int) string {
	start := strings.LastIndexByte(s[:i], '\n') + 1
	end := strings.IndexByte(s[i:], '\n')
	if end < 0 {
		return strings.TrimSpace(s[start:])
	}
	return strings.TrimSpace(s[start : i+end])
}

var errorLinePattern = regexp.MustCompile(`(?m)^.*\b(?:ERROR|FATAL|error|Error):\s*(.+)$`)

func errorLine(output string) string {
	if m := errorLinePattern.FindStringSubmatch(output); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// Summary is a one-line description: the message and its state.
func (e *SQLError) Summary() string {
	if e.Message == "" {
		return "SQLSTATE " + e.State
	}
	return fmt.Sprintf("%s (SQLSTATE %s)", e.Message, e.State)
}

// Suggestion says how to fix the migration, as specifically as the error
// allows.
func (e *SQLError) Suggestion() string {
	q := func(s string) string { return "`" + s + "`" }
	table := e.Table
	if table == "" {
		table = e.Relation
	}
	switch {
	case e.State == "23502" && strings.Contains(e.Message, "contains null values"):
		return fmt.Sprintf("Column %s was added to %s as NOT NULL without a default, but %s already has rows. "+
			"Give the column a DEFAULT, or add it as nullable, backfill it, and SET NOT NULL in a later migration.",
			q(e.Column), q(table), q(table))
	case e.State == "23502":
		where := q(e.Column)
		if table != "" {
			where = q(table + "." + e.Column)
		}
		return fmt.Sprintf("A statement wrote NULL into %s, which is NOT NULL. Supply a value or give the column a DEFAULT.", where)
	case e.State == "42701":
		return fmt.Sprintf("Column %s already exists on %s: the migration ran before or overlaps another one. "+
			"Use ADD COLUMN IF NOT EXISTS, or check the migration's version and ordering.", q(e.Column), q(table))
	case e.State == "42P07":
		return fmt.Sprintf("Relation %s already exists: the migration ran before or overlaps another one. "+
			"Use CREATE ... IF NOT EXISTS, or check the migration's version and ordering.", q(e.Relation))
	case e.State == "42601" && e.Near != "":
		return fmt.Sprintf("Fix the SQL syntax near %s, then run the migration against a local PostgreSQL before pushing.", q(e.Near))
	case e.State == "42601":
		return "The SQL ends unexpectedly (an unterminated statement or quote). Fix it and run the migration against a local PostgreSQL before pushing."
	case e.State == "42P01":
		return fmt.Sprintf("Relation %s does not exist yet: a migration that creates it must run first. Check the migrations' order.", q(e.Relation))
	case e.State == "42703":
		return fmt.Sprintf("Column %s does not exist: a migration that adds it must run first, or the name is misspelled.", q(e.Column))
	case e.State == "23505":
		return fmt.Sprintf("The statement creates duplicate values for the unique constraint %s. Deduplicate the data first, or relax the constraint.", q(e.Constraint))
	case e.State == "23503":
		return fmt.Sprintf("Rows reference values that do not exist (constraint %s). Insert the referenced rows first, or fix the reference.", q(e.Constraint))
	case e.State == "28P01" || e.State == "3D000":
		return "The migration connects with its own credentials or database name. Use the DATABASE_URL Heimdall injects."
	}
	return "Read the error above, fix the migration, and run it against a local PostgreSQL before pushing."
}
