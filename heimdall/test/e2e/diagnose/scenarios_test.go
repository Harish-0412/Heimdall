// Package diagnosee2e proves the Phase 4 exit criteria on kind (run.sh):
// each way of breaking a preview yields the expected diagnosis code and an
// actionable message, live, from real workloads. Every diagnosis code has a
// scenario here, and their snapshots are the rule fixtures
// (internal/diagnose/testdata/scenarios).
package diagnosee2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
)

type scenario struct {
	name string // fixture directory
	pr   int
	// config and images derive the scenario's inputs from the healthy ones.
	config func(string) string
	images func(map[string]string)
	// files are written next to heimdall.yaml (an import and its approval).
	files func() map[string]string
	// upArgs adds flags to `heimdall up`, given the scenario's directory.
	upArgs func(dir string) []string
	// generation of the scenario's `up` (default 1).
	generation string
	// timeout is the engine's step timeout; failing scenarios that the
	// watchdog cannot stop early (a health check that never passes, a step
	// that never finishes) use a short one.
	timeout string
	upFails bool
	// before prepares the cluster (a newer generation already deployed);
	// after breaks a healthy preview from outside (quota, database).
	before func(e *env, dir string)
	after  func(e *env, namespace string)
	// fromUp: the failure changed nothing that `heimdall diagnose --state`
	// could find later (it was refused up front), so the diagnosis `up`
	// printed, and the snapshot it saved, are what is checked.
	fromUp bool

	code       diagnose.Code
	summary    []string
	suggestion []string
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		out := strings.Replace(s, old, new, 1)
		if out == s {
			panic("scenario edit did not apply: " + old)
		}
		return out
	}
}

// The ShopFlow card: a migration adds a NOT NULL column without a default to
// a table that has rows. The rows are PostgreSQL's own catalog (database
// names), copied at runtime: no invented records (ADR 0009).
const notNullMigration = `migrations:
  service: api
  command: >-
    node --input-type=module -e "import pg from 'pg';
    const c = new pg.Client({connectionString: process.env.DATABASE_URL}); await c.connect();
    await c.query('CREATE TABLE IF NOT EXISTS catalog_snapshot AS SELECT datname FROM pg_database');
    await c.query('ALTER TABLE catalog_snapshot ADD COLUMN owner_id integer NOT NULL');
    await c.end();"
`

// An import that carries no data at all: it reads a table no migration
// creates. Approving it is honest (it is trivially sanitised), and it fails
// the way a stale import script does.
const staleImport = "-- Reads a table from an older schema.\nSELECT count(*) FROM legacy_products;\n"

func withImport(c string) string {
	return replace(`  postgres: {version: "16", storage: 1Gi}`, `  postgres: {version: "16", storage: 1Gi, seed: seed.sql}`)(c)
}

// approval attests the given bytes (ADR 0009).
func approval(of string) string {
	sum := sha256.Sum256([]byte(of))
	b, _ := json.Marshal(map[string]any{"sha256": hex.EncodeToString(sum[:]), "approvedBy": "heimdall e2e",
		"reason": "diagnostics scenario: an import with no data that reads a table no migration creates", "sanitised": true})
	return string(b)
}

func importArgs(dir string) []string {
	return []string{"--seed-approval", filepath.Join(dir, "approval.json"), "--repo-root", dir}
}

var scenarios = []scenario{
	{
		name: "image-pull", pr: 401, upFails: true, timeout: "6m",
		images: func(m map[string]string) {
			repo, _, _ := strings.Cut(m["api"], "@")
			m["api"] = repo + "@sha256:" + strings.Repeat("ab", 32) // never pushed
		},
		code: diagnose.ImagePullFailed, summary: []string{"does not exist in the registry"}, suggestion: []string{"CI pushed the image"},
	},
	{
		name: "migration-not-null", pr: 402, upFails: true, timeout: "5m",
		config:     replace("migrations:\n  service: api\n  command: npm run migrate\n", notNullMigration),
		code:       diagnose.MigrationFailed,
		summary:    []string{`column "owner_id" of relation "catalog_snapshot" contains null values (SQLSTATE 23502)`},
		suggestion: []string{"NOT NULL without a default", "DEFAULT"},
	},
	{
		name: "out-of-memory", pr: 403, upFails: true, timeout: "5m",
		config: replace("    command: npm run worker\n",
			"    command: node -e \"const a = []; setInterval(() => a.push(Buffer.alloc(16777216, 1)), 100)\"\n    resources: {memory: 128Mi}\n"),
		code: diagnose.OutOfMemory, summary: []string{"notifications was killed for exceeding its 128Mi memory limit"},
		suggestion: []string{"resources.memory"},
	},
	{
		name: "crash-loop", pr: 404, upFails: true, timeout: "5m",
		config: replace("    command: npm run worker\n", "    command: node src/notifier.js\n"),
		code:   diagnose.ContainerCrash, summary: []string{"notifications exits with code 1", "Cannot find module"},
		suggestion: []string{"heimdall logs --workload notifications"},
	},
	{
		name: "smoke-test", pr: 405, upFails: true, timeout: "5m",
		config:     replace("curl --fail http://api:8080/health", "curl --fail http://api:8080/orders/latest-report"),
		code:       diagnose.SmokeTestFailed,
		summary:    []string{`Smoke test "api-health" failed with exit code 22: GET /orders/latest-report on api returned HTTP 404`},
		suggestion: []string{"The path does not exist on api"},
	},
	{
		name: "health-check", pr: 406, upFails: true, timeout: "2m",
		config: replace("    health: {path: /health}\n    dependsOn: [postgres, redis, rabbitmq]",
			"    health: {path: /healthz}\n    dependsOn: [postgres, redis, rabbitmq]"),
		code: diagnose.HealthcheckFailed, summary: []string{"api fails its", "GET /healthz on port 8080 returns HTTP 404"},
		suggestion: []string{"`health.path` is /healthz"},
	},
	{
		name: "quota", pr: 407, timeout: "5m",
		after: func(e *env, ns string) {
			e.kubectl("-n", ns, "scale", "deployment/web", "--replicas=40")
			e.eventually("quota rejections", 90*time.Second, func() bool {
				return strings.Contains(e.kubectl("-n", ns, "get", "events", "--field-selector", "reason=FailedCreate", "-o", "name"), "event")
			})
		},
		code: diagnose.QuotaExceeded, summary: []string{"web cannot create pods: the preview's resource quota is exhausted"},
		suggestion: []string{"manual scale"},
	},
	{
		// The platform's base domain does not match the Gateway listener's
		// wildcard: every workload is Ready, the URL is dead.
		name: "route-rejected", pr: 408, timeout: "5m",
		upArgs: func(string) []string { return []string{"--base-domain", "wrong.test"} },
		after: func(e *env, ns string) {
			e.eventually("the Gateway's verdict on the route", 2*time.Minute, func() bool {
				return strings.Contains(e.kubectl("-n", ns, "get", "httproute", "web", "-o",
					`jsonpath={.status.parents[*].conditions[?(@.type=="Accepted")].status}`), "False")
			})
		},
		code:       diagnose.NoEndpoints,
		summary:    []string{"is not served: Gateway heimdall-gateway/heimdall (listener http) rejected route web (NoMatchingListenerHostname)"},
		suggestion: []string{"baseDomain"},
	},
	{
		// The platform pins previews to a node pool no node belongs to.
		name: "no-capacity", pr: 409, upFails: true, timeout: "90s",
		upArgs:     func(string) []string { return []string{"--node-selector", "heimdall.dev/pool=previews"} },
		code:       diagnose.NoCapacity,
		summary:    []string{"cannot be scheduled: no node matches the preview node pool"},
		suggestion: []string{"node pool"},
	},
	{
		name: "database-down", pr: 410, timeout: "5m",
		after: func(e *env, ns string) {
			// /health performs a real PostgreSQL query. Establish that the
			// route works before changing only the database's availability.
			e.eventually("the healthy API's database-backed health route", time.Minute, func() bool {
				return e.apiHealthStatus(ns) == "200"
			})
			e.kubectl("-n", ns, "scale", "statefulset/postgres", "--replicas=0")
			e.eventually("PostgreSQL to have no remaining pods or ready replicas", 3*time.Minute, func() bool {
				ready := strings.TrimSpace(e.kubectl("-n", ns, "get", "statefulset/postgres", "-o", "jsonpath={.status.readyReplicas}"))
				return (ready == "" || ready == "0") && strings.TrimSpace(e.kubectl("-n", ns, "get", "pods", "-l", "app.kubernetes.io/name=postgres", "-o", "name")) == ""
			})
			// A lost idle pool connection may crash the API or leave its
			// query waiting. Retry exec errors during a restart, then require
			// the previously healthy route to return a non-200 response or
			// fail a real HTTP request. Diagnosis uses PostgreSQL's typed
			// availability even when the driver prints no useful error.
			e.eventually("the API's database-backed health route to fail", 3*time.Minute, func() bool {
				status := e.apiHealthStatus(ns)
				return status != "" && status != "200"
			})
		},
		code:       diagnose.DBUnreachable,
		summary:    []string{"PostgreSQL is not running (statefulset/postgres is scaled to zero)"},
		suggestion: []string{"PostgreSQL is down"},
	},
	{
		name: "import-failed", pr: 411, upFails: true, timeout: "5m",
		config: withImport,
		files: func() map[string]string {
			return map[string]string{"seed.sql": staleImport, "approval.json": approval(staleImport)}
		},
		upArgs: importArgs,
		code:   diagnose.SeedFailed, summary: []string{`Data import failed: relation "legacy_products" does not exist (SQLSTATE 42P01)`},
		suggestion: []string{"must run first"},
	},
	{
		// The operator approved other bytes: refused before anything is
		// created.
		name: "import-not-approved", pr: 412, upFails: true, fromUp: true, timeout: "5m",
		config: withImport,
		files: func() map[string]string {
			return map[string]string{"seed.sql": staleImport, "approval.json": approval("an earlier version of the import")}
		},
		upArgs:     importArgs,
		code:       diagnose.PolicyDenied,
		summary:    []string{"approved digest does not match the imported data"},
		suggestion: []string{"approved SHA-256"},
	},
	{
		// The PR declares a secret the tenant never configured.
		name: "missing-secret", pr: 413, upFails: true, timeout: "5m",
		config: replace("    health: {path: /health}\n    dependsOn: [postgres, redis, rabbitmq]",
			"    health: {path: /health}\n    secrets: [PAYMENTS_API_KEY]\n    dependsOn: [postgres, redis, rabbitmq]"),
		code:       diagnose.ConfigInvalid,
		summary:    []string{"it needs the secret `PAYMENTS_API_KEY`, which this tenant has not configured"},
		suggestion: []string{"tenant's secrets"},
	},
	{
		// Generation 2 is already deployed; generation 1 arrives late.
		name: "stale-generation", pr: 414, upFails: true, fromUp: true, timeout: "5m",
		before: func(e *env, dir string) {
			state := filepath.Join(dir, "accepted.json")
			e.t.Cleanup(func() { _, _ = e.heimdall("down", "--state", state, "--timeout", "3m") })
			if out, err := e.heimdall(e.upCommand(filepath.Join(dir, "heimdall.yaml"), state, filepath.Join(dir, "images.json"),
				414, "2", "5m")...); err != nil {
				e.t.Fatalf("deploying generation 2: %v\n%s", err, out)
			}
		},
		generation: "1",
		code:       diagnose.StaleGeneration,
		summary:    []string{"generation 1 is older than generation 2, which this preview already accepted"},
		suggestion: []string{"newer push"},
	},
	{
		// A migration that never finishes: nothing fails, the step times out.
		name: "step-timeout", pr: 415, upFails: true, timeout: "60s",
		config:     replace("  command: npm run migrate\n", "  command: sleep 600\n"),
		code:       diagnose.Unclassified,
		summary:    []string{"step baseline-db/migrate did not finish within the step timeout: job/heimdall-migrate-g1 was still running"},
		suggestion: []string{"lock"},
	},
}

type env struct {
	t                *testing.T
	ctx, cli, images string
	fixtures, base   string
	capture          bool
	healthyImages    map[string]string
}

func TestScenarios(t *testing.T) {
	kubeContext := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if kubeContext == "" {
		t.Skip("run test/e2e/diagnose/run.sh (Docker and kind)")
	}
	if !strings.HasPrefix(kubeContext, "kind-") {
		t.Fatal("only dedicated kind contexts are supported")
	}
	base, err := os.ReadFile("../agent/heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: kubeContext, cli: os.Getenv("HEIMDALL_E2E_CLI"), images: os.Getenv("HEIMDALL_E2E_IMAGES"),
		fixtures: os.Getenv("HEIMDALL_E2E_FIXTURES"), base: string(base), capture: os.Getenv("HEIMDALL_E2E_CAPTURE") == "1"}
	b, err := os.ReadFile(e.images)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &e.healthyImages); err != nil {
		t.Fatal(err)
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sub := *e
			sub.t = t
			sub.run(sc)
		})
	}
}

func (e *env) upCommand(config, state, images string, pr int, generation, timeout string) []string {
	return []string{"up", config, "--state", state, "--repo", "local/shopflow", "--tenant", "p4", "--pr", fmt.Sprint(pr),
		"--generation", generation, "--images", images, "--sha", strings.Repeat("e", 40), "--timeout", timeout}
}

func (e *env) run(sc scenario) {
	dir := e.t.TempDir()
	config := e.base
	if sc.config != nil {
		config = sc.config(config)
	}
	images := map[string]string{}
	for k, v := range e.healthyImages {
		images[k] = v
	}
	if sc.images != nil {
		sc.images(images)
	}
	cfgPath, imgPath, state := filepath.Join(dir, "heimdall.yaml"), filepath.Join(dir, "images.json"), filepath.Join(dir, "state.json")
	e.write(cfgPath, config)
	b, _ := json.Marshal(images)
	e.write(imgPath, string(b))
	if sc.files != nil {
		for name, content := range sc.files() {
			e.write(filepath.Join(dir, name), content)
		}
	}
	e.t.Cleanup(func() {
		if _, err := os.Stat(state); os.IsNotExist(err) {
			return // A policy refusal can happen before a state file exists.
		}
		out, err := e.heimdall("down", "--state", state, "--timeout", "3m")
		if err != nil && !strings.Contains(out, "no such file") {
			e.t.Logf("down: %v\n%s", err, out)
		}
	})
	if sc.before != nil {
		sc.before(e, dir)
	}

	start := time.Now()
	generation := sc.generation
	if generation == "" {
		generation = "1"
	}
	upSnap := filepath.Join(dir, "up-snapshot.json")
	args := append(e.upCommand(cfgPath, state, imgPath, sc.pr, generation, sc.timeout), "--save-snapshot", upSnap)
	if sc.upArgs != nil {
		args = append(args, sc.upArgs(dir)...)
	}
	out, err := e.heimdall(args...)
	switch {
	case sc.upFails && err == nil:
		e.t.Fatalf("up succeeded; the scenario must break it\n%s", out)
	case !sc.upFails && err != nil:
		e.t.Fatalf("up failed: %v\n%s", err, out)
	case sc.upFails && !strings.Contains(out, string(sc.code)+": "+sc.code.Title()):
		e.t.Errorf("up did not explain its failure with %s:\n%s", sc.code, out)
	}
	e.t.Logf("up finished in %v", time.Since(start).Round(time.Second))
	namespace := e.namespace(sc.pr)
	if sc.after != nil {
		sc.after(e, namespace)
	}

	snapPath := filepath.Join(dir, "snapshot.json")
	var jsonOut string
	if sc.fromUp {
		snapPath = upSnap
		jsonOut, err = e.heimdall("diagnose", "--snapshot", snapPath, "--format", "json")
	} else {
		jsonOut, err = e.heimdall("diagnose", "--state", state, "--format", "json", "--save-snapshot", snapPath)
	}
	if code := exitCode(err); code != 1 {
		e.t.Fatalf("diagnose exit %d (want 1: problems found): %v\n%s", code, err, jsonOut)
	}
	var report diagnose.Report
	if err := json.Unmarshal([]byte(stdoutPart(jsonOut)), &report); err != nil {
		e.t.Fatalf("diagnose JSON: %v\n%s", err, jsonOut)
	}
	root := report.RootCause()
	if root == nil || root.Code != sc.code {
		e.t.Fatalf("root cause %+v, want %s; all: %+v", root, sc.code, report.Diagnoses)
	}
	for _, want := range sc.summary {
		if !strings.Contains(root.Summary, want) {
			e.t.Errorf("summary %q lacks %q", root.Summary, want)
		}
	}
	for _, want := range sc.suggestion {
		if !strings.Contains(root.Suggestion, want) {
			e.t.Errorf("suggestion %q lacks %q", root.Suggestion, want)
		}
	}
	e.t.Logf("%s: %s\n  -> %s", root.Code, root.Summary, root.Suggestion)

	md, _ := e.heimdall("diagnose", "--snapshot", snapPath, "--format", "markdown")
	if !strings.Contains(md, "### Preview failed: "+root.Title) {
		e.t.Errorf("PR comment:\n%s", md)
	}
	if namespace != "" {
		e.noSecretsIn(snapPath, namespace)
	}
	if e.capture {
		dst := filepath.Join(e.fixtures, sc.name)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			e.t.Fatal(err)
		}
		b, err := os.ReadFile(snapPath)
		if err != nil {
			e.t.Fatal(err)
		}
		e.write(filepath.Join(dst, "snapshot.json"), string(b))
		e.t.Logf("fixture captured: %s", dst)
	}
}

// apiHealthStatus reaches the process directly, even while its readiness probe
// keeps the Service from forwarding requests. Exec failures during a restart
// are transient. HTTP errors are distinct from exec errors so a hanging query
// or unreachable process can satisfy the negative gate after a healthy response.
func (e *env) apiHealthStatus(namespace string) string {
	const request = `fetch('http://127.0.0.1:8080/health', {signal: AbortSignal.timeout(5000)})
  .then(r => process.stdout.write(String(r.status), () => process.exit(0)))
  .catch(err => process.stdout.write(
    err.name === 'TimeoutError' || err.name === 'AbortError' ? 'timeout' : 'unreachable',
    () => process.exit(0)));`
	out, err := exec.Command("kubectl", "--context", e.ctx, "-n", namespace, "exec", "deployment/api", "-c", "api", "--", "node", "-e", request).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// noSecretsIn checks the snapshot for every value of the preview's
// generated credentials, as text and base64.
func (e *env) noSecretsIn(snapshot, namespace string) {
	b, err := os.ReadFile(snapshot)
	if err != nil {
		e.t.Fatal(err)
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(e.kubectl("-n", namespace, "get", "secret", "heimdall-credentials", "-o", "json")), &secret); err != nil {
		e.t.Fatal(err)
	}
	if len(secret.Data) == 0 {
		e.t.Fatal("no credentials to check against")
	}
	for key, enc := range secret.Data {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(raw) >= 6 && (bytes.Contains(b, raw) || bytes.Contains(b, []byte(enc))) {
			e.t.Errorf("credential %s appears in the snapshot", key)
		}
	}
}

// namespace finds the scenario's preview namespace, or "" when the failure
// came before it was created.
func (e *env) namespace(pr int) string {
	out := strings.TrimSpace(e.kubectl("get", "namespaces", "-l", fmt.Sprintf("heimdall.dev/pr=%d,heimdall.dev/preview=true", pr), "-o", "name"))
	if out == "" {
		return ""
	}
	return strings.TrimPrefix(strings.Fields(out)[0], "namespace/")
}

func (e *env) heimdall(args ...string) (string, error) {
	if args[0] != "diagnose" || args[1] != "--snapshot" {
		args = append(args, "--allow-context", e.ctx, "--context", e.ctx)
	}
	cmd := exec.Command(e.cli, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String() + "\n--- stderr ---\n" + stderr.String(), err
}

// stdoutPart is the stdout half of heimdall's combined output.
func stdoutPart(s string) string {
	out, _, _ := strings.Cut(s, "\n--- stderr ---\n")
	return out
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (e *env) kubectl(args ...string) string {
	e.t.Helper()
	out, err := exec.Command("kubectl", append([]string{"--context", e.ctx}, args...)...).Output()
	if err != nil {
		e.t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func (e *env) eventually(what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("no %s within %v", what, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}
