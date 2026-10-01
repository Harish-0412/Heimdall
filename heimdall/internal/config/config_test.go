package config

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalValid = `
version: 1
services:
  api:
    build: {context: .}
    port: 8080
    public: true
    health: {path: /health}
`

func load(t *testing.T, src string) (*Config, Diagnostics) {
	t.Helper()
	return Load(strings.NewReader(src), DefaultPolicy())
}

func TestLoad_MinimalIsCleanAndDefaulted(t *testing.T) {
	cfg, diags := load(t, minimalValid)
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %+v", diags)
	}
	api := cfg.Services["api"]
	if !api.Primary {
		t.Error("sole public service should become primary")
	}
	if api.Resources.CPU != defaultServiceCPU || api.Resources.Memory != defaultServiceMemory {
		t.Errorf("resources not defaulted: %+v", api.Resources)
	}
	if api.Build.Dockerfile != "Dockerfile" {
		t.Errorf("dockerfile default = %q", api.Build.Dockerfile)
	}
	if got := cfg.Preview.TTL.Std(); got != 48*time.Hour {
		t.Errorf("ttl default = %v", got)
	}
	if cfg.Preview.Visibility != VisibilityPrivate {
		t.Errorf("visibility default = %q", cfg.Preview.Visibility)
	}
	if cfg.PrimaryService() != "api" {
		t.Errorf("PrimaryService = %q", cfg.PrimaryService())
	}
}

func TestLoad_ShopFlowExample(t *testing.T) {
	f, err := os.Open("../../examples/shopflow/heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cfg, diags := Load(f, DefaultPolicy())
	if len(diags) != 0 {
		t.Fatalf("example must validate without warnings, got %+v", diags)
	}
	if got := cfg.EnabledDependencies(); !slices.Equal(got, []string{"postgres", "rabbitmq", "redis"}) {
		t.Errorf("dependencies = %v", got)
	}
	if cfg.PrimaryService() != "web" {
		t.Errorf("primary = %q", cfg.PrimaryService())
	}
	if cfg.Dependencies.Redis.Version != defaultRedisVersion {
		t.Errorf("redis version not defaulted: %q", cfg.Dependencies.Redis.Version)
	}
	if cfg.Workers["notifications"].Replicas != 1 {
		t.Error("worker replicas should default to 1")
	}
}

func TestLoad_Diagnostics(t *testing.T) {
	// wrap puts extra YAML under a valid base so each case isolates one problem.
	tests := []struct {
		name string
		src  string
		want []string // codes that must be present
	}{
		{"missing version", "services:\n  api: {build: {context: .}, port: 80, public: true, health: {path: /h}}\n", []string{"version.missing"}},
		{"unsupported version", "version: 2\n" + minimalValid[len("\nversion: 1\n"):], []string{"version.unsupported"}},
		{"no services", "version: 1\n", []string{"services.empty"}},
		{"build and image", `
version: 1
services:
  api: {build: {context: .}, image: nginx, port: 80, public: true, health: {path: /h}}
`, []string{"source.conflict"}},
		{"neither build nor image", `
version: 1
services:
  api: {port: 80, public: true, health: {path: /h}}
`, []string{"source.missing"}},
		{"context escapes repo", `
version: 1
services:
  api: {build: {context: ../secrets}, port: 80, public: true, health: {path: /h}}
`, []string{"path.invalid"}},
		{"absolute context", `
version: 1
services:
  api: {build: {context: /etc}, port: 80, public: true, health: {path: /h}}
`, []string{"path.invalid"}},
		{"missing port", `
version: 1
services:
  api: {build: {context: .}, public: true, health: {path: /h}}
`, []string{"port.missing"}},
		{"port out of range", `
version: 1
services:
  api: {build: {context: .}, port: 70000, public: true, health: {path: /h}}
`, []string{"port.invalid"}},
		{"invalid and reserved names", `
version: 1
services:
  API: {build: {context: .}, port: 80, public: true, health: {path: /h}}
  redis: {build: {context: .}, port: 80, health: {path: /h}}
`, []string{"name.invalid", "name.reserved"}},
		{"worker shares service name", `
version: 1
services:
  api: {build: {context: .}, port: 80, public: true, health: {path: /h}}
workers:
  api: {build: {context: .}, command: run}
`, []string{"name.duplicate"}},
		{"dependency cycle", `
version: 1
services:
  a: {build: {context: .}, port: 80, public: true, health: {path: /h}, dependsOn: [b]}
  b: {build: {context: .}, port: 81, health: {path: /h}, dependsOn: [a]}
`, []string{"dependsOn.cycle"}},
		{"unknown dependsOn", `
version: 1
services:
  a: {build: {context: .}, port: 80, public: true, health: {path: /h}, dependsOn: [ghost]}
`, []string{"dependsOn.unknown"}},
		{"dependsOn disabled dependency", `
version: 1
services:
  a: {build: {context: .}, port: 80, public: true, health: {path: /h}, dependsOn: [postgres]}
`, []string{"dependsOn.unknown"}},
		{"dependsOn worker", `
version: 1
services:
  a: {build: {context: .}, port: 80, public: true, health: {path: /h}, dependsOn: [w]}
workers:
  w: {build: {context: .}, command: run}
`, []string{"dependsOn.worker"}},
		{"migrations without postgres", minimalValid + `
migrations: {service: api, command: run}
`, []string{"migrations.requires_postgres"}},
		{"migrations unknown service", minimalValid + `
dependencies: {postgres: {}}
migrations: {service: nope, command: run}
`, []string{"migrations.service.unknown"}},
		{"reserved env prefix", `
version: 1
services:
  api:
    build: {context: .}
    port: 80
    public: true
    health: {path: /h}
    env: {HEIMDALL_URL: x}
`, []string{"env.reserved"}},
		{"secret and env clash", `
version: 1
services:
  api:
    build: {context: .}
    port: 80
    public: true
    health: {path: /h}
    env: {STRIPE_KEY: x}
    secrets: [STRIPE_KEY]
`, []string{"secret.conflict"}},
		{"bad cpu", `
version: 1
services:
  api: {build: {context: .}, port: 80, public: true, health: {path: /h}, resources: {cpu: lots}}
`, []string{"resources.invalid"}},
		{"memory too large", `
version: 1
services:
  api: {build: {context: .}, port: 80, public: true, health: {path: /h}, resources: {memory: 64Gi}}
`, []string{"resources.too_large"}},
		{"ttl too long", minimalValid + "preview: {ttl: 30d}\n", []string{"preview.ttl.out_of_range"}},
		{"bad duration", minimalValid + "preview: {ttl: soon}\n", []string{"value.invalid"}},
		{"sleepAfter beyond ttl", minimalValid + "preview: {ttl: 2h, sleepAfter: 3h}\n", []string{"preview.sleepAfter.out_of_range"}},
		{"invalid visibility", minimalValid + "preview: {visibility: everyone}\n", []string{"preview.visibility.invalid"}},
		{"two public no primary", `
version: 1
services:
  web: {build: {context: .}, port: 80, public: true, health: {path: /h}}
  api: {build: {context: .}, port: 81, public: true, health: {path: /h}}
`, []string{"service.primary.ambiguous"}},
		{"primary not public", `
version: 1
services:
  api: {build: {context: .}, port: 80, primary: true, health: {path: /h}}
`, []string{"service.primary.not_public"}},
		{"null dependency", minimalValid + "dependencies:\n  postgres:\n", []string{"yaml.null"}},
		{"unsupported postgres", minimalValid + "dependencies: {postgres: {version: '9'}}\n", []string{"postgres.version.unsupported"}},
		{"seed not sql", minimalValid + "dependencies: {postgres: {seed: dump.txt}}\n", []string{"seed.extension"}},
		{"worker without command", minimalValid + "workers:\n  w: {build: {context: .}}\n", []string{"worker.command.missing"}},
		{"unknown field typo", `
version: 1
services:
  api:
    build: {context: .}
    port: 80
    dependson: [x]
`, []string{"field.unknown"}},
		{"wrong type", `
version: 1
services:
  api: {build: {context: .}, port: eighty}
`, []string{"type.mismatch"}},
		{"yaml syntax", "version: 1\nservices: [unclosed\n", []string{"yaml.syntax"}},
		{"empty file", "# nothing here\n", []string{"file.empty"}},
		{"multiple documents", minimalValid + "---\nversion: 1\n", []string{"file.multiple_documents"}},
		{"cpu quota", `
version: 1
services:
  a: {build: {context: .}, port: 80, public: true, health: {path: /h}, resources: {cpu: 2}}
  b: {build: {context: .}, port: 81, health: {path: /h}, resources: {cpu: 2}}
  c: {build: {context: .}, port: 82, health: {path: /h}, resources: {cpu: 2}}
  d: {build: {context: .}, port: 83, health: {path: /h}, resources: {cpu: 2}}
`, []string{"quota.cpu"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags := load(t, tc.src)
			if cfg != nil {
				t.Fatalf("expected failure, got a valid config (diags: %+v)", diags)
			}
			got := diags.Codes()
			for _, code := range tc.want {
				if !slices.Contains(got, code) {
					t.Errorf("missing code %q; got %v", code, got)
				}
			}
		})
	}
}

func TestLoad_Warnings(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"literal secret", `
version: 1
services:
  api:
    build: {context: .}
    port: 80
    public: true
    health: {path: /h}
    env: {DB_PASSWORD: hunter2}
`, "env.secret_literal"},
		{"no public service", `
version: 1
services:
  api: {build: {context: .}, port: 80, health: {path: /h}}
`, "service.public.none"},
		{"no health check", `
version: 1
services:
  api: {build: {context: .}, port: 80, public: true}
`, "health.missing"},
		{"public visibility", minimalValid + "preview: {visibility: public}\n", "preview.visibility.public"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags := load(t, tc.src)
			if cfg == nil {
				t.Fatalf("warnings must not invalidate a config: %+v", diags)
			}
			if !slices.Contains(diags.Codes(), tc.want) {
				t.Errorf("want warning %q, got %v", tc.want, diags.Codes())
			}
			if diags.HasErrors() {
				t.Errorf("unexpected errors: %+v", diags)
			}
		})
	}
}

func TestLoad_FileTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("# padding\n"), MaxFileSize/10+10)
	cfg, diags := Load(bytes.NewReader(big), DefaultPolicy())
	if cfg != nil || !slices.Contains(diags.Codes(), "file.too_large") {
		t.Fatalf("want file.too_large, got %+v", diags)
	}
}

func TestLoad_ReportsLineNumbers(t *testing.T) {
	src := "version: 1\nservices:\n  api:\n    build: {context: .}\n    port: 70000\n    public: true\n    health: {path: /h}\n"
	_, diags := load(t, src)
	if len(diags) != 1 || diags[0].Code != "port.invalid" {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if diags[0].Line != 5 {
		t.Errorf("line = %d, want 5", diags[0].Line)
	}
}

func TestLoad_UnknownFieldSuggestsCorrection(t *testing.T) {
	_, diags := load(t, "version: 1\nservices:\n  api:\n    build: {context: .}\n    port: 80\n    dependson: [x]\n")
	var d *Diagnostic
	for i := range diags {
		if diags[i].Code == "field.unknown" {
			d = &diags[i]
		}
	}
	if d == nil {
		t.Fatalf("no field.unknown in %+v", diags)
	}
	if !strings.Contains(d.Hint, "dependsOn") {
		t.Errorf("hint = %q, want suggestion for dependsOn", d.Hint)
	}
	if d.Line != 6 {
		t.Errorf("line = %d, want 6", d.Line)
	}
}

func TestLoad_UnknownFieldDoesNotHideOtherErrors(t *testing.T) {
	_, diags := load(t, "version: 1\nservices:\n  api:\n    build: {context: .}\n    dependson: [x]\n")
	got := diags.Codes()
	for _, want := range []string{"field.unknown", "port.missing"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"48h": 48 * time.Hour, "2d": 48 * time.Hour, "90m": 90 * time.Minute, "1h30m": 90 * time.Minute,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0h", "-1h", "10", "soon", "d", "99999d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) should fail", in)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		48 * time.Hour: "2d", 36 * time.Hour: "36h", 90 * time.Minute: "90m", 90 * time.Second: "1m30s",
	}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestQuantities(t *testing.T) {
	cpu := map[string]int{"250m": 250, "1": 1000, "0.5": 500, "1.5": 1500}
	for in, want := range cpu {
		if got, err := parseCPU(in); err != nil || got != want {
			t.Errorf("parseCPU(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "0m", "NaN", "1e9"} {
		if _, err := parseCPU(in); err == nil {
			t.Errorf("parseCPU(%q) should fail", in)
		}
	}

	mem := map[string]int{"256Mi": 256, "1Gi": 1024, "2Gi": 2048}
	for in, want := range mem {
		if got, err := parseMebibytes(in); err != nil || got != want {
			t.Errorf("parseMebibytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0Mi", "1G", "512", "-1Gi", "lots"} {
		if _, err := parseMebibytes(in); err == nil {
			t.Errorf("parseMebibytes(%q) should fail", in)
		}
	}
}

func TestCheckRelPath(t *testing.T) {
	for _, p := range []string{".", "api", "./api", "a/b/../c", "fixtures/dev.sql"} {
		if err := checkRelPath(p); err != nil {
			t.Errorf("checkRelPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "..", "../x", "a/../../x", "/etc", `C:\x`, "C:/x", `a\b`} {
		if err := checkRelPath(p); err == nil {
			t.Errorf("checkRelPath(%q) should fail", p)
		}
	}
}

func TestLevenshteinAndClosest(t *testing.T) {
	if d := levenshtein("kitten", "sitting"); d != 3 {
		t.Errorf("levenshtein = %d, want 3", d)
	}
	if got := closest("dependson", []string{"port", "dependsOn"}); got != "dependsOn" {
		t.Errorf("closest = %q", got)
	}
	if got := closest("zzzzzz", []string{"port", "dependsOn"}); got != "" {
		t.Errorf("closest = %q, want empty", got)
	}
}
