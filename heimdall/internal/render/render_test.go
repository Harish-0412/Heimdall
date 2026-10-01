package render

import (
	"bytes"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const minimalSrc = "version: 1\nservices:\n  app: {build: {context: .}, port: 8080, public: true, health: {path: /h}}\n"

func renderErrors(t *testing.T, cfg *config.Config, ctx Context) Errors {
	t.Helper()
	_, err := Render(cfg, ctx)
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("want render.Errors, got %v", err)
	}
	return errs
}

func TestRender_RejectsUnloadedConfig(t *testing.T) {
	cfg := loadString(t, minimalSrc)
	errs := renderErrors(t, &config.Config{Services: cfg.Services}, testContext(t, cfg))
	if !slices.Equal(errs.Codes(), []string{CodeConfigNotLoaded}) {
		t.Fatalf("codes = %v", errs.Codes())
	}
}

func TestRender_ContextValidation(t *testing.T) {
	cfg := loadString(t, minimalSrc)
	tests := map[string]func(*Context){
		"Tenant":               func(c *Context) { c.Tenant = "Acme Corp" },
		"Repo":                 func(c *Context) { c.Repo = "no-slash" },
		"Repo dot":             func(c *Context) { c.Repo = "acme/.." },
		"PR":                   func(c *Context) { c.PR = 0 },
		"SHA":                  func(c *Context) { c.SHA = "abc123" },
		"Generation":           func(c *Context) { c.Generation = 0 },
		"EnvironmentID":        func(c *Context) { c.EnvironmentID = "Env_1" },
		"Owner":                func(c *Context) { c.Owner = "not a login" },
		"ExpiresAt":            func(c *Context) { c.ExpiresAt = time.Time{} },
		"URLSuffix":            func(c *Context) { c.URLSuffix = "XY" },
		"Policy":               func(c *Context) { c.Policy = config.Policy{} },
		"Credentials":          func(c *Context) { c.Credentials = &Credentials{PostgresSuperuser: "short"} },
		"BaseDomain missing":   func(c *Context) { c.Platform.BaseDomain = "" },
		"BaseDomain single":    func(c *Context) { c.Platform.BaseDomain = "localhost" },
		"URLScheme":            func(c *Context) { c.Platform.URLScheme = "ftp" },
		"URLPort":              func(c *Context) { c.Platform.URLPort = 70000 },
		"Gateway":              func(c *Context) { c.Platform.Gateway = GatewayRef{Namespace: "Bad NS", Name: "gw"} },
		"EgressCIDRs parse":    func(c *Context) { c.Platform.EgressCIDRs = []string{"10.0.0.0/33"} },
		"EgressCIDRs hostbits": func(c *Context) { c.Platform.EgressCIDRs = []string{"10.0.0.1/8"} },
		"EgressCIDRs metadata": func(c *Context) { c.Platform.EgressCIDRs = []string{"169.254.169.254/32"} },
		"ImageMirror":          func(c *Context) { c.Platform.ImageMirror = "https://mirror.example.com" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t, cfg)
			edit(&ctx)
			errs := renderErrors(t, cfg, ctx)
			if len(errs) == 0 || (errs[0].Code != CodeContextInvalid && errs[0].Code != CodeCredentialsInvalid) {
				t.Fatalf("codes = %v", errs.Codes())
			}
		})
	}
}

func TestRender_ReportsAllContextProblemsAtOnce(t *testing.T) {
	cfg := loadString(t, minimalSrc)
	ctx := testContext(t, cfg)
	ctx.Tenant, ctx.PR, ctx.SHA = "", 0, ""
	if errs := renderErrors(t, cfg, ctx); len(errs) != 3 {
		t.Fatalf("want 3 errors, got %v", errs)
	}
}

func TestRender_Images(t *testing.T) {
	const pinned = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name   string
		src    string
		images map[string]string
		want   string // error code, or "" for success
	}{
		{"built and pinned", minimalSrc, map[string]string{"app": "ghcr.io/acme/app:v1" + pinned}, ""},
		{"registry with port", minimalSrc, map[string]string{"app": "localhost:5001/app" + pinned}, ""},
		{"missing", minimalSrc, nil, CodeImageMissing},
		{"tag only", minimalSrc, map[string]string{"app": "ghcr.io/acme/app:v1"}, CodeImageUnpinned},
		{"uppercase", minimalSrc, map[string]string{"app": "GHCR.io/acme/app" + pinned}, CodeImageUnpinned},
		{"latest tag", minimalSrc, map[string]string{"app": "ghcr.io/acme/app:latest" + pinned}, CodeImageLatest},
		{"unknown workload", minimalSrc, map[string]string{"app": "a/b" + pinned, "ghost": "a/b" + pinned}, CodeImageUnknown},
		{"declared and pinned in config", strings.Replace(minimalSrc, "build: {context: .}", "image: nginx:1.27"+pinned, 1), nil, ""},
		{"declared tag needs a digest", strings.Replace(minimalSrc, "build: {context: .}", "image: nginx:1.27", 1), nil, CodeImageMissing},
		{"resolved digest of declared image", strings.Replace(minimalSrc, "build: {context: .}", "image: nginx:1.27", 1),
			map[string]string{"app": "docker.io/library/nginx" + pinned}, ""},
		{"resolved to another repository", strings.Replace(minimalSrc, "build: {context: .}", "image: ghcr.io/acme/app:1", 1),
			map[string]string{"app": "docker.io/evil/miner" + pinned}, CodeImageMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadString(t, tc.src)
			ctx := testContext(t, cfg)
			ctx.Images = tc.images
			plan, err := Render(cfg, ctx)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var errs Errors
			if !errors.As(err, &errs) || !slices.Contains(errs.Codes(), tc.want) {
				t.Fatalf("want %s, got plan=%v err=%v", tc.want, plan != nil, err)
			}
		})
	}
}

func TestRepository(t *testing.T) {
	tests := map[string]string{
		"nginx":                          "docker.io/library/nginx",
		"nginx:1.27":                     "docker.io/library/nginx",
		"acme/app:1@sha256:abc":          "docker.io/acme/app",
		"index.docker.io/acme/app":       "docker.io/acme/app",
		"ghcr.io/acme/app:v1":            "ghcr.io/acme/app",
		"localhost:5001/app":             "localhost:5001/app",
		"localhost/app:dev":              "localhost/app",
		"registry.example.com:443/a/b:c": "registry.example.com:443/a/b",
	}
	for in, want := range tests {
		if got := repository(in); got != want {
			t.Errorf("repository(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender_Seed(t *testing.T) {
	withSeed := loadString(t, minimalSrc+"dependencies: {postgres: {seed: db/seed.sql}}\n")
	without := loadString(t, minimalSrc+"dependencies: {postgres: {}}\n")
	tests := []struct {
		name string
		cfg  *config.Config
		seed []byte
		want string
	}{
		{"missing", withSeed, nil, CodeSeedMissing},
		{"unexpected", without, []byte("SELECT 1;"), CodeSeedUnexpected},
		{"too large", withSeed, bytes.Repeat([]byte("x"), MaxSeedBytes+1), CodeSeedTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t, tc.cfg)
			ctx.Seed = tc.seed
			if errs := renderErrors(t, tc.cfg, ctx); !slices.Equal(errs.Codes(), []string{tc.want}) {
				t.Fatalf("codes = %v", errs.Codes())
			}
		})
	}
	t.Run("binary seed uses binaryData", func(t *testing.T) {
		ctx := testContext(t, withSeed)
		ctx.Seed = []byte{0xff, 0xfe, 'x'}
		cm := find[*corev1.ConfigMap](mustRender(t, withSeed, ctx), "heimdall-seed-g3")
		if cm == nil || cm.BinaryData[seedKey] == nil || cm.Data != nil || !*cm.Immutable {
			t.Fatalf("seed ConfigMap = %+v", cm)
		}
	})
}

func TestNames(t *testing.T) {
	if got := namespaceName(184, "acme/shopflow", "x7d2"); got != "heimdall-pr184-shopflow-x7d2" {
		t.Errorf("namespace = %s", got)
	}
	if got := hostLabel(184, "acme/shopflow", "", "x7d2"); got != "pr184-shopflow-x7d2" {
		t.Errorf("primary host = %s", got)
	}
	if got := hostLabel(184, "acme/shopflow", "api", "x7d2"); got != "pr184-shopflow-api-x7d2" {
		t.Errorf("service host = %s", got)
	}
	long := hostLabel(99_999_999, "acme/"+strings.Repeat("very-long-repository-name", 4), strings.Repeat("s", 40), "abcdefgh")
	if len(long) > 63 || !strings.HasSuffix(long, "-"+strings.Repeat("s", 40)+"-abcdefgh") {
		t.Errorf("long host = %s (%d)", long, len(long))
	}
	if got := hostLabel(1, "acme/Shop_Flow.V2", "", "abcd"); got != "pr1-shop-flow-v2-abcd" {
		t.Errorf("sanitized host = %s", got)
	}
	a, b := objectName("heimdall-smoke", strings.Repeat("a", 60)), objectName("heimdall-smoke", strings.Repeat("a", 59)+"b")
	if len(a) > 63 || a == b {
		t.Errorf("truncated names must stay unique and short: %s %s", a, b)
	}
	if got := labelValue("acme.shopflow"); got != "acme.shopflow" {
		t.Errorf("valid label values are kept: %s", got)
	}
	if got := envVarSuffix("my-api"); got != "MY_API" {
		t.Errorf("envVarSuffix = %s", got)
	}
}

func TestRender_URLsAndEnv(t *testing.T) {
	plan, _ := shopflowPlan(t)
	want := []URL{
		{Service: "api", URL: "https://pr184-shopflow-api-x7d2.preview.example.com"},
		{Service: "web", Primary: true, URL: "https://pr184-shopflow-x7d2.preview.example.com"},
	}
	if !slices.Equal(plan.URLs, want) {
		t.Fatalf("URLs = %+v", plan.URLs)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range find[*appsv1.Deployment](plan, "web").Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	for name, value := range map[string]string{
		"PORT": "3000", EnvPR: "184", EnvSHA: "4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39",
		EnvPublicURL: want[1].URL, EnvPublicURLPrefix + "API": want[0].URL, EnvPublicURLPrefix + "WEB": want[1].URL,
	} {
		if env[name].Value != value {
			t.Errorf("%s = %q, want %q", name, env[name].Value, value)
		}
	}
	for name, key := range map[string]string{"DATABASE_URL": keyDatabaseURL, "REDIS_URL": keyRedisURL, "AMQP_URL": keyAMQPURL} {
		if ref := env[name].ValueFrom; ref == nil || ref.SecretKeyRef.Name != CredentialsSecret || ref.SecretKeyRef.Key != key {
			t.Errorf("%s must come from %s/%s, got %+v", name, CredentialsSecret, key, env[name])
		}
	}
	migrate := find[*batchv1.Job](plan, "heimdall-migrate-g3").Spec.Template.Spec.Containers[0]
	for _, e := range migrate.Env {
		if e.Name == "DATABASE_URL" && e.ValueFrom.SecretKeyRef.Key != keyDatabaseURLBaseline {
			t.Errorf("migrations must target the baseline database, got %s", e.ValueFrom.SecretKeyRef.Key)
		}
	}
}

func TestRender_Credentials(t *testing.T) {
	plan, _ := shopflowPlan(t)
	s := find[*corev1.Secret](plan, CredentialsSecret)
	if s == nil {
		t.Fatal("credentials secret not rendered")
	}
	want := map[string]string{
		keyDatabaseURL:         "postgres://app:PostgresApp0000000000000@postgres:5432/app?sslmode=disable",
		keyDatabaseURLBaseline: "postgres://app:PostgresApp0000000000000@postgres:5432/app_baseline?sslmode=disable",
		keyRedisURL:            "redis://:Redis0000000000000000000@redis:6379/0",
		keyAMQPURL:             "amqp://app:RabbitMQ0000000000000000@rabbitmq:5672/%2F",
	}
	for k, v := range want {
		if string(s.Data[k]) != v {
			t.Errorf("%s = %q, want %q", k, s.Data[k], v)
		}
	}

	cfg := loadString(t, minimalSrc)
	ctx := testContext(t, cfg)
	if find[*corev1.Secret](mustRender(t, cfg, ctx), CredentialsSecret) != nil {
		t.Error("no dependencies, so no credentials secret")
	}
	if c := GenerateCredentials(); c.PostgresSuperuser == c.PostgresApp || !passwordRE.MatchString(c.Redis) {
		t.Errorf("generated credentials are not usable: %+v", c)
	}
}

func TestRender_QuotaAndLimitRange(t *testing.T) {
	plan, ctx := shopflowPlan(t)
	q := find[*corev1.ResourceQuota](plan, quotaName).Spec.Hard
	// Policy ceiling plus the largest Job step: the migration (api: 500m/512Mi).
	checks := map[corev1.ResourceName]string{
		corev1.ResourceLimitsCPU:              "6500m",
		corev1.ResourceLimitsMemory:           "12800Mi",
		corev1.ResourcePods:                   "53", // 8 services + 8 workers x 5 + 3 dependencies + 2 smoke Jobs
		corev1.ResourcePersistentVolumeClaims: "1",
		corev1.ResourceServicesLoadBalancers:  "0",
		corev1.ResourceServicesNodePorts:      "0",
	}
	for name, want := range checks {
		if got := q[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("quota %s = %s, want %s", name, got.String(), want)
		}
	}
	lr := find[*corev1.LimitRange](plan, limitRangeName).Spec.Limits[0]
	if got := lr.MaxLimitRequestRatio[corev1.ResourceMemory]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("memory ratio = %s", got.String())
	}

	// A policy whose per-container ceiling is below the medium preset must
	// still produce a LimitRange the API server accepts (default <= max).
	ctx.Policy.MaxContainerCPUMilli, ctx.Policy.MaxContainerMemoryMi = 250, 256
	b := &builder{cfg: loadString(t, minimalSrc), ctx: ctx, platform: ctx.Platform.withDefaults()}
	small := b.limitRange().Spec.Limits[0]
	for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		def, maxQ, req := small.Default[r], small.Max[r], small.DefaultRequest[r]
		if def.Cmp(maxQ) > 0 || req.Cmp(def) > 0 {
			t.Errorf("%s: defaultRequest %s <= default %s <= max %s violated", r, req.String(), def.String(), maxQ.String())
		}
	}
}

func TestRender_NetworkPolicies(t *testing.T) {
	plan, _ := shopflowPlan(t)
	var names []string
	for _, o := range plan.Objects() {
		if _, ok := o.(*networkingv1.NetworkPolicy); ok {
			names = append(names, o.GetName())
		}
	}
	want := []string{"heimdall-allow-dns", "heimdall-allow-gateway-api", "heimdall-allow-gateway-web", "heimdall-allow-same-namespace", "heimdall-default-deny"}
	if !slices.Equal(names, want) {
		t.Fatalf("policies = %v, want %v (no egress allowlist by default)", names, want)
	}
	gw := find[*networkingv1.NetworkPolicy](plan, "heimdall-allow-gateway-web")
	if gw.Spec.Ingress[0].Ports[0].Port.IntValue() != 3000 || gw.Spec.PodSelector.MatchLabels[labelName] != "web" {
		t.Errorf("gateway policy must admit only web's port: %+v", gw.Spec)
	}
	if got := metadataExcepts("0.0.0.0/0"); !slices.Equal(got, []string{"169.254.0.0/16"}) {
		t.Errorf("excepts(0.0.0.0/0) = %v", got)
	}
	if got := metadataExcepts("10.0.0.0/8"); got != nil {
		t.Errorf("excepts(10.0.0.0/8) = %v", got)
	}
}

func TestRender_StagesAndSteps(t *testing.T) {
	plan, _ := shopflowPlan(t)
	var steps []string
	for _, s := range plan.Stages {
		for _, st := range s.Steps {
			steps = append(steps, string(s.Name)+"/"+st.Name)
		}
	}
	want := []string{
		"guardrails/setup", "dependencies/start",
		"baseline-db/prepare", "baseline-db/migrate", "baseline-db/seed", "baseline-db/clone",
		"application/wave-1", "application/wave-2", "smoke/run",
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %v", steps)
	}
	first := plan.Stages[0].Steps[0].Objects[0]
	if first.GetObjectKind().GroupVersionKind().Kind != "Namespace" {
		t.Error("the namespace must be applied first")
	}

	minimal := mustRender(t, loadString(t, minimalSrc), testContext(t, loadString(t, minimalSrc)))
	if len(minimal.Stages) != 5 {
		t.Fatal("every plan lists all five stages")
	}
	for _, s := range []StageName{StageDependencies, StageBaselineDB, StageSmoke} {
		if st, _ := minimal.Stage(s); len(st.Steps) != 0 {
			t.Errorf("stage %s should be empty for a minimal config", s)
		}
	}
}

func TestRender_GenerationNamesAndLabels(t *testing.T) {
	plan, _ := shopflowPlan(t)
	for _, o := range plan.Objects() {
		if o.GetLabels()[LabelGeneration] != "3" {
			t.Errorf("%s/%s lacks the generation label", kindOf(o), o.GetName())
		}
	}
	// Long-running pods must not restart just because the generation changed.
	api := find[*appsv1.Deployment](plan, "api")
	if _, ok := api.Spec.Template.Labels[LabelGeneration]; ok {
		t.Error("deployment pod templates must not carry the generation")
	}
	if find[*batchv1.Job](plan, "heimdall-smoke-api-health-g3") == nil {
		t.Error("smoke jobs are named per generation")
	}
}

func TestEncode(t *testing.T) {
	plan, _ := shopflowPlan(t)
	var buf bytes.Buffer
	if err := WritePlan(&buf, plan, ""); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{"status:", "creationTimestamp", "updateStrategy: {}"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
	if err := WritePlan(&buf, plan, "nope"); err == nil {
		t.Error("unknown stage must be an error")
	}

	buf.Reset()
	minimal := mustRender(t, loadString(t, minimalSrc), testContext(t, loadString(t, minimalSrc)))
	if err := WritePlan(&buf, minimal, StageBaselineDB); err != nil || !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("empty stage output = %q, %v", buf.String(), err)
	}

	files, err := Files(plan)
	if err != nil {
		t.Fatal(err)
	}
	nameRE := regexp.MustCompile(`^\d{2}-[a-z-]+-[a-z0-9-]+\.yaml$`)
	for i, f := range files {
		if !nameRE.MatchString(f.Name) || (i > 0 && f.Name <= files[i-1].Name) {
			t.Errorf("file %q is badly named or out of order", f.Name)
		}
	}
	if files[0].Name != "01-guardrails-setup.yaml" || files[len(files)-1].Name != "09-smoke-run.yaml" {
		t.Errorf("files = %s ... %s", files[0].Name, files[len(files)-1].Name)
	}
}

func TestCatalog(t *testing.T) {
	refRE := regexp.MustCompile(`^[a-z0-9./-]+/[a-z0-9/-]+:[a-z0-9.-]+@sha256:[a-f0-9]{64}$`)
	for _, dep := range []string{config.DepPostgres, config.DepRedis, config.DepRabbitMQ} {
		supported := config.SupportedVersions(dep)
		images := dependencyImages(dep)
		for _, v := range supported {
			img, ok := images[v]
			if !ok {
				t.Errorf("%s %s is supported by config but has no pinned image", dep, v)
				continue
			}
			if !refRE.MatchString(img.ref("")) || !refRE.MatchString(img.ref("mirror.example.com/hub")) || img.uid <= 0 || img.gid <= 0 {
				t.Errorf("%s %s: bad catalog entry %+v", dep, v, img)
			}
			if !strings.HasPrefix(img.tag, v) {
				t.Errorf("%s %s: tag %s is another version", dep, v, img.tag)
			}
		}
		for v := range images {
			if !slices.Contains(supported, v) {
				t.Errorf("%s %s is pinned but config does not accept it", dep, v)
			}
		}
	}
	if !refRE.MatchString(toolboxImage.ref("")) {
		t.Errorf("toolbox ref %s", toolboxImage.ref(""))
	}
}
