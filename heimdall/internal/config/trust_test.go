package config

import (
	"slices"
	"strings"
	"testing"
)

const baselineSrc = `
version: 1
services:
  api:
    build: {context: .}
    port: 8080
    public: true
    health: {path: /health}
    secrets: [STRIPE_TEST_KEY]
    resources: {cpu: 500m, memory: 512Mi}
dependencies:
  postgres: {storage: 1Gi}
preview: {ttl: 24h, visibility: private}
`

func mustLoad(t *testing.T, src string) *Config {
	t.Helper()
	cfg, diags := load(t, src)
	if cfg == nil {
		t.Fatalf("expected valid config, got %+v", diags)
	}
	return cfg
}

func TestPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
		src    string
		want   string
	}{
		{"visibility above max", func() Policy { p := DefaultPolicy(); p.MaxVisibility = VisibilityOrg; return p }(),
			minimalValid + "preview: {visibility: public}\n", "policy.visibility_denied"},
		{"secret not allowed", func() Policy { p := DefaultPolicy(); p.AllowedSecrets = []string{"OTHER"}; return p }(),
			strings.Replace(minimalValid, "public: true", "public: true\n    secrets: [STRIPE_TEST_KEY]", 1), "policy.secret_denied"},
		{"no secrets allowed at all", func() Policy { p := DefaultPolicy(); p.AllowedSecrets = []string{}; return p }(),
			strings.Replace(minimalValid, "public: true", "public: true\n    secrets: [STRIPE_TEST_KEY]", 1), "policy.secret_denied"},
		{"registry not allowed", func() Policy { p := DefaultPolicy(); p.AllowedRegistries = []string{"ghcr.io/shopflow/"}; return p }(),
			"version: 1\nservices:\n  api: {image: docker.io/evil/miner:latest, port: 80, public: true, health: {path: /h}}\n", "policy.image_denied"},
		{"prebuilt images disabled", func() Policy { p := DefaultPolicy(); p.AllowedRegistries = []string{}; return p }(),
			"version: 1\nservices:\n  api: {image: nginx, port: 80, public: true, health: {path: /h}}\n", "policy.image_denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags := Load(strings.NewReader(tc.src), tc.policy)
			if cfg != nil || !slices.Contains(diags.Codes(), tc.want) {
				t.Fatalf("want %q, got cfg=%v diags=%v", tc.want, cfg != nil, diags.Codes())
			}
		})
	}
}

func TestPolicy_AllowedValuesPass(t *testing.T) {
	p := DefaultPolicy()
	p.AllowedSecrets = []string{"STRIPE_TEST_KEY"}
	p.AllowedRegistries = []string{"ghcr.io/shopflow/"}
	p.MaxVisibility = VisibilityOrg
	src := `
version: 1
services:
  api:
    image: ghcr.io/shopflow/api@sha256:abc
    port: 80
    public: true
    health: {path: /h}
    secrets: [STRIPE_TEST_KEY]
preview: {visibility: org}
`
	if cfg, diags := Load(strings.NewReader(src), p); cfg == nil {
		t.Fatalf("expected valid, got %+v", diags)
	}
}

func TestCompareToBaseline_IdenticalAndTighterPass(t *testing.T) {
	base := mustLoad(t, baselineSrc)
	if d := CompareToBaseline(base, mustLoad(t, baselineSrc)); len(d) != 0 {
		t.Fatalf("identical config must pass, got %+v", d)
	}
	tighter := strings.NewReplacer("ttl: 24h", "ttl: 2h", "512Mi", "256Mi", "cpu: 500m", "cpu: 250m").Replace(baselineSrc)
	if d := CompareToBaseline(base, mustLoad(t, tighter)); len(d) != 0 {
		t.Fatalf("tightening must pass, got %+v", d)
	}
}

func TestCompareToBaseline_AppLevelChangesAreAllowed(t *testing.T) {
	pr := `
version: 1
services:
  api:
    build: {context: services/api}
    port: 9090
    public: true
    health: {path: /healthz}
    env: {FEATURE_COUPONS: "true"}
    secrets: [STRIPE_TEST_KEY]
    resources: {cpu: 500m, memory: 512Mi}
workers:
  mailer: {build: {context: .}, command: npm run mail}
dependencies:
  postgres: {storage: 1Gi}
  redis: {}
migrations: {service: api, command: npm run migrate:v2}
preview: {ttl: 24h, visibility: private}
`
	if d := CompareToBaseline(mustLoad(t, baselineSrc), mustLoad(t, pr)); len(d) != 0 {
		t.Fatalf("application-level changes must pass, got %+v", d)
	}
}

func TestCompareToBaseline_Denials(t *testing.T) {
	tests := []struct {
		name string
		edit *strings.Replacer
		want string
	}{
		{"more open visibility", strings.NewReplacer("visibility: private", "visibility: org"), "trust.visibility"},
		{"longer ttl", strings.NewReplacer("ttl: 24h", "ttl: 72h"), "trust.ttl"},
		{"new secret", strings.NewReplacer("[STRIPE_TEST_KEY]", "[STRIPE_TEST_KEY, AWS_ADMIN_KEY]"), "trust.secret"},
		{"more memory", strings.NewReplacer("memory: 512Mi", "memory: 2Gi"), "trust.resources"},
		{"more cpu", strings.NewReplacer("cpu: 500m", "cpu: 2"), "trust.resources"},
		{"bigger database", strings.NewReplacer("storage: 1Gi", "storage: 5Gi"), "trust.resources"},
	}
	base := mustLoad(t, baselineSrc)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := CompareToBaseline(base, mustLoad(t, tc.edit.Replace(baselineSrc)))
			if !slices.Contains(d.Codes(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, d.Codes())
			}
			if d[0].Line == 0 {
				t.Errorf("trust diagnostics should carry a line number: %+v", d[0])
			}
		})
	}
}

func TestCompareToBaseline_NewWorkloadCappedAtDefaults(t *testing.T) {
	pr := baselineSrc + `
workers:
  crunch: {build: {context: .}, command: run, resources: {cpu: 2, memory: 4Gi}}
`
	d := CompareToBaseline(mustLoad(t, baselineSrc), mustLoad(t, pr))
	if !slices.Contains(d.Codes(), "trust.resources") {
		t.Fatalf("want trust.resources, got %v", d.Codes())
	}
}
