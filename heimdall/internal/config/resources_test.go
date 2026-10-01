package config

import (
	"slices"
	"strings"
	"testing"
)

// svc renders a one-service config whose service body is extra.
func svc(extra string) string {
	return "version: 1\nservices:\n  api:\n    build: {context: .}\n    port: 8080\n    public: true\n    health: {path: /h}\n" + extra
}

func TestResources_PresetsAndDefaults(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want Resources
	}{
		{"service default is medium", svc(""),
			Resources{CPU: "500m", Memory: "512Mi", Requests: Requests{CPU: "100m", Memory: "256Mi"}}},
		{"small", svc("    resources: {size: small}\n"),
			Resources{Size: "small", CPU: "250m", Memory: "256Mi", Requests: Requests{CPU: "50m", Memory: "128Mi"}}},
		{"medium", svc("    resources: {size: medium}\n"),
			Resources{Size: "medium", CPU: "500m", Memory: "512Mi", Requests: Requests{CPU: "100m", Memory: "256Mi"}}},
		{"large", svc("    resources: {size: large}\n"),
			Resources{Size: "large", CPU: "1", Memory: "2Gi", Requests: Requests{CPU: "200m", Memory: "1024Mi"}}},
		{"memory floor 64Mi", svc("    resources: {cpu: 20m, memory: 100Mi}\n"),
			Resources{CPU: "20m", Memory: "100Mi", Requests: Requests{CPU: "10m", Memory: "64Mi"}}},
		{"request never above a tiny limit", svc("    resources: {cpu: 5m, memory: 32Mi}\n"),
			Resources{CPU: "5m", Memory: "32Mi", Requests: Requests{CPU: "5m", Memory: "32Mi"}}},
		{"odd limit rounds the request up", svc("    resources: {memory: 513Mi}\n"),
			Resources{CPU: "500m", Memory: "513Mi", Requests: Requests{CPU: "100m", Memory: "257Mi"}}},
		{"explicit requests kept", svc("    resources: {cpu: 1, memory: 1Gi, requests: {cpu: 1, memory: 768Mi}}\n"),
			Resources{CPU: "1", Memory: "1Gi", Requests: Requests{CPU: "1", Memory: "768Mi"}}},
		{"partial requests", svc("    resources: {requests: {cpu: 300m}}\n"),
			Resources{CPU: "500m", Memory: "512Mi", Requests: Requests{CPU: "300m", Memory: "256Mi"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoad(t, tc.src)
			if got := cfg.Services["api"].Resources; got != tc.want {
				t.Errorf("resources = %+v\nwant        %+v", got, tc.want)
			}
		})
	}
}

func TestResources_WorkerDefaultIsSmall(t *testing.T) {
	cfg := mustLoad(t, svc("workers:\n  w: {build: {context: .}, command: run}\n"))
	want := Resources{CPU: "250m", Memory: "256Mi", Requests: Requests{CPU: "50m", Memory: "128Mi"}}
	if got := cfg.Workers["w"].Resources; got != want {
		t.Errorf("worker resources = %+v, want %+v", got, want)
	}
}

func TestResources_StricterPolicyRaisesDefaultMemoryRequest(t *testing.T) {
	p := DefaultPolicy()
	p.MinMemoryRequestPercent = 70
	cfg, diags := Load(strings.NewReader(svc("")), p)
	if cfg == nil {
		t.Fatalf("defaults must satisfy the policy that produced them: %+v", diags)
	}
	if got := cfg.Services["api"].Resources.Requests.Memory; got != "359Mi" { // ceil(512 * 0.7)
		t.Errorf("memory request = %s, want 359Mi", got)
	}
}

func TestResources_Diagnostics(t *testing.T) {
	tests := []struct {
		name   string
		policy func(*Policy)
		src    string
		want   string
	}{
		{"unknown size", nil, svc("    resources: {size: huge}\n"), "resources.size.invalid"},
		{"size with cpu", nil, svc("    resources: {size: small, cpu: 1}\n"), "resources.size.conflict"},
		{"size with requests", nil, svc("    resources: {size: small, requests: {memory: 200Mi}}\n"), "resources.size.conflict"},
		{"large needs approval", func(p *Policy) { p.AllowLargeSize = false }, svc("    resources: {size: large}\n"), "policy.size_denied"},
		{"bad request", nil, svc("    resources: {requests: {cpu: lots}}\n"), "resources.invalid"},
		{"cpu request above limit", nil, svc("    resources: {cpu: 250m, requests: {cpu: 500m}}\n"), "resources.requests.exceeds_limit"},
		{"memory request above defaulted limit", nil, svc("    resources: {requests: {memory: 600Mi}}\n"), "resources.requests.exceeds_limit"},
		{"memory over-commit", nil, svc("    resources: {memory: 2Gi, requests: {memory: 512Mi}}\n"), "resources.requests.too_low"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			if tc.policy != nil {
				tc.policy(&p)
			}
			cfg, diags := Load(strings.NewReader(tc.src), p)
			if cfg != nil || !slices.Contains(diags.Codes(), tc.want) {
				t.Fatalf("want %q, got valid=%v %v", tc.want, cfg != nil, diags.Codes())
			}
			for _, d := range diags {
				if d.Code == tc.want && d.Line == 0 {
					t.Errorf("%s has no line number: %+v", d.Code, d)
				}
			}
		})
	}
}

func TestResources_ZeroPolicyDeniesLarge(t *testing.T) {
	// A tenant policy that forgets the field must fail closed.
	p := Policy{Limits: DefaultLimits(), MaxVisibility: VisibilityPrivate}
	if _, diags := Load(strings.NewReader(svc("    resources: {size: large}\n")), p); !slices.Contains(diags.Codes(), "policy.size_denied") {
		t.Fatalf("want policy.size_denied, got %v", diags.Codes())
	}
}

func TestInjectedEnvIsReserved(t *testing.T) {
	tests := []struct {
		name string
		src  string
		bad  bool
	}{
		{"PORT on a service", svc("    env: {PORT: '3000'}\n"), true},
		{"PORT on a worker is fine", svc("workers:\n  w: {build: {context: .}, command: run, env: {PORT: '1'}}\n"), false},
		{"DATABASE_URL with postgres", svc("    env: {DATABASE_URL: x}\ndependencies: {postgres: {}}\n"), true},
		{"DATABASE_URL without postgres", svc("    env: {DATABASE_URL: x}\n"), false},
		{"secret named REDIS_URL", svc("    secrets: [REDIS_URL]\ndependencies: {redis: {}}\n"), true},
		{"AMQP_URL on a worker", svc("workers:\n  w: {build: {context: .}, command: run, env: {AMQP_URL: x}}\ndependencies: {rabbitmq: {}}\n"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags := load(t, tc.src)
			got := slices.Contains(diags.Codes(), "env.reserved")
			if got != tc.bad || (cfg == nil) != tc.bad {
				t.Fatalf("env.reserved=%v valid=%v, want reserved=%v; %v", got, cfg != nil, tc.bad, diags.Codes())
			}
		})
	}
}

func TestReservedNamePrefix(t *testing.T) {
	_, diags := load(t, "version: 1\nservices:\n  heimdall-db: {build: {context: .}, port: 80, public: true, health: {path: /h}}\n")
	if !slices.Contains(diags.Codes(), "name.reserved") {
		t.Fatalf("want name.reserved, got %v", diags.Codes())
	}
}

func TestCompareToBaseline_SizePresetsResolveBeforeComparison(t *testing.T) {
	base := mustLoad(t, svc(""))
	if d := CompareToBaseline(base, mustLoad(t, svc("    resources: {size: medium}\n"))); len(d) != 0 {
		t.Fatalf("medium equals the default; got %v", d.Codes())
	}
	d := CompareToBaseline(base, mustLoad(t, svc("    resources: {size: large}\n")))
	if !slices.Contains(d.Codes(), "trust.resources") {
		t.Fatalf("large exceeds the default branch; got %v", d.Codes())
	}
}

func TestDependencyResourcesAndVersions(t *testing.T) {
	got := DependencyResources(DepPostgres, DefaultPolicy())
	want := Resources{CPU: "500m", Memory: "512Mi", Requests: Requests{CPU: "100m", Memory: "256Mi"}}
	if got != want {
		t.Errorf("postgres resources = %+v, want %+v", got, want)
	}
	if v := SupportedVersions(DepRedis); !slices.Contains(v, defaultRedisVersion) {
		t.Errorf("redis default %q not in supported %v", defaultRedisVersion, v)
	}
	if SupportedVersions("mysql") != nil {
		t.Error("unknown dependency should have no versions")
	}
	// The returned slice is a copy; callers cannot widen what Load accepts.
	SupportedVersions(DepPostgres)[0] = "9"
	if supportedPostgres[0] == "9" {
		t.Error("SupportedVersions leaked its backing array")
	}
}

func TestLoaded(t *testing.T) {
	if (&Config{}).Loaded() || (*Config)(nil).Loaded() {
		t.Error("hand-built configs are not loaded")
	}
	if !mustLoad(t, minimalValid).Loaded() {
		t.Error("Load output must report Loaded")
	}
}
