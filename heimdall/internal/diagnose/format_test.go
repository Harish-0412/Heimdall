package diagnose

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() *Report {
	return &Report{Namespace: "heimdall-pr184-shopflow-1f3a", Generation: 2, Diagnoses: []Diagnosis{
		{Code: MigrationFailed, Title: MigrationFailed.Title(), Severity: SeverityError, Stage: "baseline-db",
			Subject: "job/heimdall-migrate-g2", Workload: "heimdall-migrate",
			Summary:    `Migration failed: column "owner_id" of relation "catalog" contains null values (SQLSTATE 23502)`,
			Suggestion: "Column `owner_id` was added to `catalog` as NOT NULL without a default, but `catalog` already has rows.",
			Evidence:   []string{`error: column "owner_id" of relation "catalog" contains null values`, "  code: '23502',"}},
		{Code: HealthcheckFailed, Title: HealthcheckFailed.Title(), Severity: SeverityError, Stage: "application",
			Subject: "deployment/api", Summary: "api fails its readiness check: GET /health on port 8080 returns HTTP 503",
			Suggestion: "The app is up but reports itself unhealthy."},
	}}
}

// Exact text: PR comments are a public contract (reviewers and bots read
// them); any change to them must be deliberate.
func TestMarkdownComment(t *testing.T) {
	var b bytes.Buffer
	if err := WriteMarkdown(&b, sampleReport(), CommentContext{Environment: "heimdall-pr184-shopflow-1f3a", Generation: 2,
		Commit: "3829272448149dd91fa559778c1411feec2d0cee"}); err != nil {
		t.Fatal(err)
	}
	want := "### Preview failed: Database migration failed\n\n" +
		"**`MIGRATION_FAILED`** in `job/heimdall-migrate-g2` (stage: `baseline-db`)\n\n" +
		"Migration failed: column \"owner\\_id\" of relation \"catalog\" contains null values (SQLSTATE 23502)\n\n" +
		"**What to do:** Column `owner_id` was added to `catalog` as NOT NULL without a default, but `catalog` already has rows.\n" +
		"\n<details><summary>Evidence</summary>\n\n" +
		"```text\nerror: column \"owner_id\" of relation \"catalog\" contains null values\n  code: '23502',\n```\n" +
		"\n</details>\n" +
		"\n<details><summary>Also found (1)</summary>\n\n" +
		"- **`HEALTHCHECK_FAILED`** api fails its readiness check: GET /health on port 8080 returns HTTP 503  \n" +
		"  The app is up but reports itself unhealthy.\n" +
		"\n</details>\n" +
		"\n<sub>`heimdall-pr184-shopflow-1f3a` · generation 2 · commit `382927244814` · run `heimdall diagnose` for details</sub>\n"
	if got := b.String(); got != want {
		t.Errorf("comment differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Log lines and identifiers come from the app: they must not be able to
// close the evidence block, inject HTML or turn into links and formatting.
func TestMarkdownCannotBeInjected(t *testing.T) {
	r := &Report{Diagnoses: []Diagnosis{{Code: ContainerCrash, Title: ContainerCrash.Title(),
		Subject:    "deployment/api`</details>",
		Summary:    "api exits: <img src=x onerror=alert(1)> **bold** [click](https://evil.example) | table # head",
		Suggestion: "Unbalanced `backtick and <script>",
		Evidence:   []string{"```", "</details><script>alert(1)</script>", "````more"}}}}
	var b bytes.Buffer
	if err := WriteMarkdown(&b, r, CommentContext{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	prose := outsideCode(out)
	for _, bad := range []string{"<img", "<script>", "**bold**", "[click](", "</details><script>"} {
		if strings.Contains(prose, bad) {
			t.Errorf("%q survived outside code:\n%s", bad, prose)
		}
	}
	// The evidence fence is longer than any backtick run inside it.
	if !strings.Contains(out, "`````text\n```\n</details><script>alert(1)</script>\n````more\n`````\n") {
		t.Errorf("evidence fence:\n%s", out)
	}
	if !strings.Contains(out, "``deployment/api`</details>``") {
		t.Errorf("subject code span:\n%s", out)
	}
}

// outsideCode is markdown with fenced blocks and code spans removed: what
// GitHub would interpret.
func outsideCode(md string) string {
	var out []string
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		switch {
		case fence != "" && line == fence:
			fence = ""
		case fence != "":
		case strings.HasPrefix(line, "```"):
			fence = strings.TrimRight(line, "abcdefghijklmnopqrstuvwxyz")
		default:
			out = append(out, line)
		}
	}
	text := strings.Join(out, "\n")
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '`' {
			b.WriteByte(text[i])
			i++
			continue
		}
		n := 0
		for i+n < len(text) && text[i+n] == '`' {
			n++
		}
		end := strings.Index(text[i+n:], strings.Repeat("`", n))
		if end < 0 {
			b.WriteString(text[i:])
			break
		}
		i += n + end + n
	}
	return b.String()
}

func TestText(t *testing.T) {
	var b bytes.Buffer
	if err := WriteText(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	want := `MIGRATION_FAILED: Database migration failed (job/heimdall-migrate-g2, stage baseline-db)
  Migration failed: column "owner_id" of relation "catalog" contains null values (SQLSTATE 23502)

  What to do: Column ` + "`owner_id` was added to `catalog` as NOT NULL without a default, but `catalog` already has rows." + `

  Evidence:
    | error: column "owner_id" of relation "catalog" contains null values
    |   code: '23502',

Also found (1):
  - HEALTHCHECK_FAILED api fails its readiness check: GET /health on port 8080 returns HTTP 503
      The app is up but reports itself unhealthy.
`
	if got := b.String(); got != want {
		t.Errorf("text differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	b.Reset()
	_ = WriteText(&b, &Report{})
	if b.String() != "No problems found.\n" {
		t.Errorf("empty: %q", b.String())
	}
}

func TestJSONIsStable(t *testing.T) {
	var b bytes.Buffer
	if err := WriteJSON(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Diagnoses[0].Code != MigrationFailed || strings.Contains(b.String(), `<`) {
		t.Errorf("json:\n%s", b.String())
	}
}
