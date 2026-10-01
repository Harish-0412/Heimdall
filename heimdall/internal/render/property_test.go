package render

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// randomConfig generates a heimdall.yaml exercising random combinations of
// features. Services only depend on lower-numbered services, so the graph is
// acyclic by construction; a quota failure is the only expected rejection.
func randomConfig(r *rand.Rand) string {
	var b strings.Builder
	pick := func(n int) bool { return r.IntN(n) == 0 }
	deps := map[string]bool{config.DepPostgres: pick(2), config.DepRedis: pick(2), config.DepRabbitMQ: pick(2)}
	var enabled []string
	for _, d := range []string{config.DepPostgres, config.DepRabbitMQ, config.DepRedis} {
		if deps[d] {
			enabled = append(enabled, d)
		}
	}
	resources := func() string {
		switch r.IntN(4) {
		case 0:
			return ""
		case 1:
			return fmt.Sprintf("    resources: {size: %s}\n", []string{"small", "medium", "large"}[r.IntN(3)])
		case 2:
			return fmt.Sprintf("    resources: {cpu: %dm, memory: %dMi}\n", 50+r.IntN(900), 64+r.IntN(900))
		default:
			lim := 128 + r.IntN(800)
			return fmt.Sprintf("    resources: {cpu: 1, memory: %dMi, requests: {cpu: %dm, memory: %dMi}}\n", lim, 10+r.IntN(990), (lim+1)/2+r.IntN(lim/2))
		}
	}
	source := func(name string) string {
		switch r.IntN(3) {
		case 0:
			return fmt.Sprintf("    image: ghcr.io/acme/%s:%d.%d\n", name, r.IntN(5), r.IntN(20))
		case 1:
			return fmt.Sprintf("    image: ghcr.io/acme/%s@sha256:%064x\n", name, r.Uint64())
		}
		return fmt.Sprintf("    build: {context: %s}\n", name)
	}
	someOf := func(from []string) []string {
		var out []string
		for _, s := range from {
			if pick(2) {
				out = append(out, s)
			}
		}
		return out
	}

	b.WriteString("version: 1\nservices:\n")
	nServices := 1 + r.IntN(4)
	var services []string
	publicSeen := false
	for i := range nServices {
		name := fmt.Sprintf("svc-%c%d", 'a'+rune(r.IntN(26)), i)
		fmt.Fprintf(&b, "  %s:\n%s    port: %d\n", name, source(name), 1024+r.IntN(60000))
		if pick(2) {
			b.WriteString("    public: true\n")
			if !publicSeen {
				b.WriteString("    primary: true\n")
				publicSeen = true
			}
		}
		if pick(2) {
			fmt.Fprintf(&b, "    health: {path: /h%d, initialDelay: %ds}\n", i, 1+r.IntN(60))
		}
		if pick(2) {
			fmt.Fprintf(&b, "    env: {FEATURE_%d: \"on\", LOG_LEVEL: debug}\n", i)
		}
		if pick(3) {
			b.WriteString("    secrets: [STRIPE_TEST_KEY]\n")
		}
		b.WriteString(resources())
		if d := append(someOf(services), someOf(enabled)...); len(d) > 0 {
			fmt.Fprintf(&b, "    dependsOn: [%s]\n", strings.Join(d, ", "))
		}
		services = append(services, name)
	}

	if nWorkers := r.IntN(3); nWorkers > 0 {
		b.WriteString("workers:\n")
		for i := range nWorkers {
			name := fmt.Sprintf("worker-%d", i)
			fmt.Fprintf(&b, "  %s:\n%s    command: run --queue q%d\n    replicas: %d\n", name, source(name), i, 1+r.IntN(2))
			if d := append(someOf(services), someOf(enabled)...); len(d) > 0 {
				fmt.Fprintf(&b, "    dependsOn: [%s]\n", strings.Join(d, ", "))
			}
		}
	}

	if len(enabled) > 0 {
		b.WriteString("dependencies:\n")
		for _, d := range enabled {
			versions := config.SupportedVersions(d)
			fmt.Fprintf(&b, "  %s: {version: %q", d, versions[r.IntN(len(versions))])
			if d == config.DepPostgres {
				fmt.Fprintf(&b, ", storage: %dGi", 1+r.IntN(4))
				if pick(2) {
					b.WriteString(", seed: fixtures/seed.sql")
				}
			}
			b.WriteString("}\n")
		}
	}
	if deps[config.DepPostgres] && pick(2) {
		fmt.Fprintf(&b, "migrations: {service: %s, command: make migrate}\n", services[r.IntN(len(services))])
	}
	if n := r.IntN(4); n > 0 {
		b.WriteString("smokeTests:\n")
		for i := range n {
			fmt.Fprintf(&b, "  - {name: check-%d, command: curl -fsS http://%s/}\n", i, services[r.IntN(len(services))])
		}
	}
	fmt.Fprintf(&b, "preview: {ttl: %dh, visibility: %s}\n", 1+r.IntN(160), []string{"private", "org", "public"}[r.IntN(3)])
	return b.String()
}

// TestProperty_RandomConfigs renders many random configs and checks, for each:
// byte-identical output across renders (determinism), the expected structure,
// and every security invariant.
func TestProperty_RandomConfigs(t *testing.T) {
	iterations := 400
	if testing.Short() {
		iterations = 50
	}
	rendered := 0
	for seed := range uint64(iterations) {
		r := rand.New(rand.NewPCG(seed, 0x4845494d44414c4c)) // "HEIMDALL"
		src := randomConfig(r)
		cfg, diags := config.Load(strings.NewReader(src), config.DefaultPolicy())
		if cfg == nil {
			for _, d := range diags {
				if d.Severity == config.SeverityError && !strings.HasPrefix(d.Code, "quota.") {
					t.Fatalf("seed %d: generator produced an invalid config (%v):\n%s", seed, diags.Codes(), src)
				}
			}
			continue
		}
		ctx := testContext(t, cfg)
		ctx.Generation = int64(1 + r.IntN(1_000_000))
		ctx.PR = 1 + r.IntN(maxPR)

		first := mustRender(t, cfg, ctx)
		var a, b bytes.Buffer
		if err := WritePlan(&a, first, ""); err != nil {
			t.Fatal(err)
		}
		// Go randomizes map iteration; a second render exercises other orders.
		if err := WritePlan(&b, mustRender(t, cfg, ctx), ""); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Fatalf("seed %d: rendering is not deterministic:\n%s", seed, firstDiff(a.Bytes(), b.Bytes()))
		}
		for _, v := range checkPlan(first, ctx) {
			t.Errorf("seed %d: %s\n%s", seed, v, src)
		}
		checkStructure(t, seed, cfg, first)
		rendered++
	}
	if rendered < iterations*3/4 {
		t.Fatalf("only %d of %d random configs were valid; the generator drifted", rendered, iterations)
	}
}

func checkStructure(t *testing.T, seed uint64, cfg *config.Config, p *Plan) {
	t.Helper()
	names := make([]StageName, len(p.Stages))
	for i, s := range p.Stages {
		names[i] = s.Name
	}
	if !slices.Equal(names, StageNames()) {
		t.Errorf("seed %d: stages %v, want %v", seed, names, StageNames())
	}
	for name, w := range cfg.Workers {
		if d := find[*appsv1.Deployment](p, name); d == nil || *d.Spec.Replicas != int32(w.Replicas) {
			t.Errorf("seed %d: worker %s missing or wrong replicas", seed, name)
		}
	}
	for name, s := range cfg.Services {
		if find[*appsv1.Deployment](p, name) == nil {
			t.Errorf("seed %d: service %s has no Deployment", seed, name)
		}
		if route := find[*gatewayv1.HTTPRoute](p, name); (route != nil) != s.Public {
			t.Errorf("seed %d: service %s public=%v but route present=%v", seed, name, s.Public, route != nil)
		}
	}
	// A workload never starts before a service it depends on.
	wave := map[string]int{}
	app, _ := p.Stage(StageApplication)
	for i, st := range app.Steps {
		for _, o := range st.Objects {
			if _, ok := o.(*appsv1.Deployment); ok {
				wave[o.GetName()] = i
			}
		}
	}
	check := func(name string, deps []string) {
		for _, d := range deps {
			if _, isService := cfg.Services[d]; isService && wave[d] >= wave[name] {
				t.Errorf("seed %d: %s (wave %d) does not start after %s (wave %d)", seed, name, wave[name]+1, d, wave[d]+1)
			}
		}
	}
	for name, s := range cfg.Services {
		check(name, s.DependsOn)
	}
	for name, w := range cfg.Workers {
		check(name, w.DependsOn)
	}
}

// Fuzz the name builders: whatever the input, outputs must be valid and
// bounded, or the API server rejects the whole step.

func FuzzLabelValue(f *testing.F) {
	for _, s := range []string{"", "acme/shopflow", "dependabot[bot]", strings.Repeat("x", 300), "-_.", "é", "a..b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := labelValue(s)
		if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
			t.Fatalf("labelValue(%q) = %q: %v", s, got, errs)
		}
		if validation.IsValidLabelValue(s) == nil && got != s {
			t.Fatalf("valid value %q was changed to %q", s, got)
		}
	})
}

func FuzzNames(f *testing.F) {
	f.Add(184, "acme/shopflow", "api", "x7d2", "migrate", int64(3))
	f.Add(99_999_999, "o/"+strings.Repeat("Long.Repo_Name", 10), strings.Repeat("s", 40), "abcdefgh", strings.Repeat("t", 40), int64(1)<<62)
	f.Add(1, "o/---", "", "0000", "", int64(1))
	f.Fuzz(func(t *testing.T, pr int, repo, service, suffix, part string, gen int64) {
		if pr < 1 || pr > maxPR || !suffixRE.MatchString(suffix) || len(service) > 40 || len(part) > 40 {
			t.Skip()
		}
		if service != "" && len(validation.IsDNS1123Label(service)) > 0 {
			t.Skip()
		}
		ns := namespaceName(pr, repo, suffix)
		host := hostLabel(pr, repo, service, suffix)
		job := generationName(gen, "heimdall-smoke", dnsWord(part))
		for _, name := range []string{ns, host, job} {
			if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
				t.Fatalf("invalid DNS label %q from (%d, %q, %q, %q): %v", name, pr, repo, service, suffix, errs)
			}
		}
		if !strings.HasSuffix(ns, "-"+suffix) || !strings.HasSuffix(host, "-"+suffix) {
			t.Fatalf("suffix %q lost: %q %q", suffix, ns, host)
		}
		if service != "" && !strings.Contains(host, "-"+service+"-") {
			t.Fatalf("service %q lost from host %q", service, host)
		}
	})
}
