// Package diagnose explains why a preview failed and what to do about it.
//
// Diagnose is a pure function over a Snapshot (pods, events, Jobs, workload
// status, endpoints, quota usage and redacted log tails, as Collect captures
// them): the same snapshot always yields the same ranked diagnoses, so rules
// are tested against snapshots captured from real clusters
// (testdata/scenarios). Each Diagnosis carries a stable public Code, a
// specific summary, an actionable suggestion and short redacted evidence.
// Renderers produce the CLI text, the pull-request comment and JSON
// (docs/diagnostics.md).
package diagnose

// Code is a stable public diagnosis code. Dashboards, PR comments, alerts and
// the control plane key on it: never rename or reuse one, only add.
type Code string

// The codes, in rank order (see rank.go).
const (
	ConfigInvalid     Code = "CONFIG_INVALID"
	PolicyDenied      Code = "POLICY_DENIED"
	StaleGeneration   Code = "STALE_GENERATION"
	QuotaExceeded     Code = "QUOTA_EXCEEDED"
	NoCapacity        Code = "NO_CAPACITY"
	ImagePullFailed   Code = "IMAGE_PULL_FAILED"
	DBUnreachable     Code = "DB_UNREACHABLE"
	MigrationFailed   Code = "MIGRATION_FAILED"
	SeedFailed        Code = "SEED_FAILED"
	OutOfMemory       Code = "OUT_OF_MEMORY"
	ContainerCrash    Code = "CONTAINER_CRASH"
	HealthcheckFailed Code = "HEALTHCHECK_FAILED"
	NoEndpoints       Code = "NO_ENDPOINTS"
	SmokeTestFailed   Code = "SMOKE_TEST_FAILED"
	// Unclassified reports a failure no rule explains, with the engine's own
	// code and message, so a failure is never reported without a diagnosis.
	Unclassified Code = "UNCLASSIFIED"
)

// Severity of a diagnosis.
type Severity string

// Severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type codeInfo struct {
	title string
	// tier orders causes within a stage: inputs, then what prevents starting,
	// then data, then runtime, then readiness, then verification.
	tier int
}

var catalog = map[Code]codeInfo{
	ConfigInvalid:     {"Configuration is invalid", 0},
	PolicyDenied:      {"Denied by policy", 0},
	StaleGeneration:   {"Superseded by a newer generation", 0},
	QuotaExceeded:     {"Resource quota exceeded", 1},
	NoCapacity:        {"No capacity to schedule", 1},
	ImagePullFailed:   {"Image cannot be pulled", 1},
	DBUnreachable:     {"Database unreachable", 2},
	MigrationFailed:   {"Database migration failed", 2},
	SeedFailed:        {"Data import failed", 2},
	OutOfMemory:       {"Out of memory", 3},
	ContainerCrash:    {"Container crashes", 3},
	HealthcheckFailed: {"Health check failing", 4},
	NoEndpoints:       {"Service has no ready endpoints", 4},
	SmokeTestFailed:   {"Smoke test failed", 5},
	Unclassified:      {"Failed", 6},
}

// order is the position of each code in the const block above.
var order = func() map[Code]int {
	m := map[Code]int{}
	for i, c := range Codes() {
		m[c] = i
	}
	return m
}()

// Codes returns every code in rank order.
func Codes() []Code {
	return []Code{ConfigInvalid, PolicyDenied, StaleGeneration, QuotaExceeded, NoCapacity, ImagePullFailed,
		DBUnreachable, MigrationFailed, SeedFailed, OutOfMemory, ContainerCrash, HealthcheckFailed, NoEndpoints,
		SmokeTestFailed, Unclassified}
}

// Title is the code's short human title.
func (c Code) Title() string { return catalog[c].title }
