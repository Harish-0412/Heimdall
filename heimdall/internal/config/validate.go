package config

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	nameRE       = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	envKeyRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretRE     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	imageRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-/:@]{0,254}$`)
	secretLikeRE = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key)`)
	driveRE      = regexp.MustCompile(`^[A-Za-z]:`)
)

// Names that would shadow a managed dependency's DNS name or a platform name
// inside the preview namespace.
var reservedNames = map[string]bool{
	DepPostgres: true, DepRedis: true, DepRabbitMQ: true,
	"heimdall": true, "kubernetes": true,
}

// reservedPrefix is kept for objects the platform creates in a preview
// namespace (jobs, policies, config maps), so a workload's name and selector
// can never collide with one of them.
const reservedPrefix = "heimdall-"

const (
	MaxWorkerReplicas  = 5
	maxSmokeTests      = 10
	maxMigrationTime   = 30 // minutes
	maxSmokeTestTime   = 10 // minutes
	minSleepAfterMin   = 10 // minutes
	maxHealthDelaySecs = 300
)

// validator accumulates diagnostics. It never stops at the first problem: a
// user fixing a config should see everything wrong in one pass.
type validator struct {
	cfg    *Config
	sm     *SourceMap
	policy Policy
	diags  Diagnostics
}

func (v *validator) add(sev Severity, code, p, hint, format string, args ...any) {
	pos := v.sm.Lookup(p)
	v.diags = append(v.diags, Diagnostic{
		Severity: sev,
		Code:     code,
		Path:     p,
		Line:     pos.Line,
		Column:   pos.Column,
		Message:  fmt.Sprintf(format, args...),
		Hint:     hint,
	})
}

func (v *validator) errorf(code, p, hint, format string, args ...any) {
	v.add(SeverityError, code, p, hint, format, args...)
}

func (v *validator) warnf(code, p, hint, format string, args ...any) {
	v.add(SeverityWarning, code, p, hint, format, args...)
}

func (v *validator) run() {
	v.version()
	v.names()
	v.services()
	v.workers()
	v.dependencies()
	v.migrations()
	v.smokeTests()
	v.preview()
	v.graph()
}

// nulls rejects "postgres:" with no value. yaml.v3 decodes it to a nil
// pointer, which would silently mean "disabled" - the opposite of intent.
func (v *validator) nulls(doc *yaml.Node) {
	if len(doc.Content) == 0 {
		return
	}
	deps := mappingValue(doc.Content[0], "dependencies")
	if deps == nil || deps.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(deps.Content); i += 2 {
		k, val := deps.Content[i], deps.Content[i+1]
		if val.Kind == yaml.ScalarNode && val.ShortTag() == "!!null" {
			v.errorf("yaml.null", "dependencies."+k.Value,
				fmt.Sprintf("Write `%s: {}` to enable it with defaults, or remove the key.", k.Value),
				"dependency %q has no value", k.Value)
		}
	}
}

func (v *validator) version() {
	switch v.cfg.Version {
	case CurrentVersion:
	case 0:
		v.errorf("version.missing", "version", "Add `version: 1` at the top of the file.", "version is required")
	default:
		v.errorf("version.unsupported", "version", fmt.Sprintf("Supported version: %d.", CurrentVersion),
			"unsupported version %d", v.cfg.Version)
	}
}

func (v *validator) names() {
	for _, n := range sortedKeys(v.cfg.Services) {
		v.checkName("services."+n, n)
	}
	for _, n := range sortedKeys(v.cfg.Workers) {
		v.checkName("workers."+n, n)
		if _, dup := v.cfg.Services[n]; dup {
			v.errorf("name.duplicate", "workers."+n,
				"Services and workers share one namespace; rename one of them.",
				"worker %q has the same name as a service", n)
		}
	}
}

func (v *validator) checkName(p, name string) {
	switch {
	case !nameRE.MatchString(name):
		v.errorf("name.invalid", p,
			"Use lowercase letters, digits and '-'; start with a letter; at most 40 characters.",
			"invalid name %q", name)
	case reservedNames[name]:
		v.errorf("name.reserved", p,
			"This name is used by a managed dependency or the platform.",
			"name %q is reserved", name)
	case strings.HasPrefix(name, reservedPrefix):
		v.errorf("name.reserved", p,
			"The heimdall- prefix is reserved for objects the platform creates.",
			"name %q uses a reserved prefix", name)
	}
}

func (v *validator) services() {
	if len(v.cfg.Services) == 0 {
		v.errorf("services.empty", "services", "Declare at least one service with a build and a port.", "no services defined")
		return
	}
	if len(v.cfg.Services) > v.policy.MaxServices {
		v.errorf("services.too_many", "services", "", "%d services declared; the limit is %d",
			len(v.cfg.Services), v.policy.MaxServices)
	}

	var public, primary []string
	for _, name := range sortedKeys(v.cfg.Services) {
		s := v.cfg.Services[name]
		base := "services." + name

		v.source(base, s.Build, s.Image)
		v.port(base, s.Port)
		v.health(base+".health", s.Health)
		v.env(base, s.Env)
		v.secrets(base, s.Secrets, s.Env)
		v.injected(base, true, s.Env, s.Secrets)
		v.resources(base+".resources", s.Resources)

		if s.Health == nil {
			v.warnf("health.missing", base,
				"Add `health: {path: /health}` so Heimdall waits for the app, not just the port.",
				"%s has no HTTP health check; a TCP probe will be used", base)
		}
		if s.Public {
			public = append(public, name)
		}
		if s.Primary {
			primary = append(primary, name)
			if !s.Public {
				v.errorf("service.primary.not_public", base+".primary", "Set `public: true` as well.",
					"%s is primary but not public", base)
			}
		}
	}

	switch {
	case len(public) == 0:
		v.warnf("service.public.none", "services",
			"Set `public: true` on the service reviewers should open.",
			"no service is public, so the preview will have no URL")
	case len(primary) > 1:
		v.errorf("service.primary.multiple", "services", "Keep `primary: true` on one service only.",
			"%d services are marked primary: %s", len(primary), strings.Join(primary, ", "))
	case len(public) > 1 && len(primary) == 0:
		v.errorf("service.primary.ambiguous", "services",
			"The primary service gets the bare URL; the others get pr-N-<service> URLs.",
			"%d services are public (%s); mark exactly one with `primary: true`",
			len(public), strings.Join(public, ", "))
	}
}

func (v *validator) workers() {
	if len(v.cfg.Workers) > v.policy.MaxWorkers {
		v.errorf("workers.too_many", "workers", "", "%d workers declared; the limit is %d",
			len(v.cfg.Workers), v.policy.MaxWorkers)
	}
	for _, name := range sortedKeys(v.cfg.Workers) {
		w := v.cfg.Workers[name]
		base := "workers." + name

		v.source(base, w.Build, w.Image)
		v.env(base, w.Env)
		v.secrets(base, w.Secrets, w.Env)
		v.injected(base, false, w.Env, w.Secrets)
		v.resources(base+".resources", w.Resources)

		if strings.TrimSpace(w.Command) == "" {
			v.errorf("worker.command.missing", base, "Example: `command: npm run worker`.",
				"%s needs a command", base)
		}
		if w.Replicas < 0 || w.Replicas > MaxWorkerReplicas {
			v.errorf("worker.replicas.out_of_range", base+".replicas", "",
				"replicas must be between 1 and %d (omit for 1)", MaxWorkerReplicas)
		}
	}
}

// source checks that exactly one of build/image is set and that each is sane.
func (v *validator) source(base string, b *Build, image string) {
	switch {
	case b == nil && image == "":
		v.errorf("source.missing", base,
			"Set `build.context` to build from the repo, or `image` for a prebuilt image.",
			"%s must set either build or image", base)
	case b != nil && image != "":
		v.errorf("source.conflict", base+".image", "Remove one of `build` or `image`.",
			"%s sets both build and image", base)
	}

	if b != nil {
		if b.Context != "" {
			if err := checkRelPath(b.Context); err != nil {
				v.errorf("path.invalid", base+".build.context", "", "build context %q %v", b.Context, err)
			}
		}
		if b.Dockerfile != "" {
			if err := checkRelPath(b.Dockerfile); err != nil {
				v.errorf("path.invalid", base+".build.dockerfile", "", "dockerfile %q %v", b.Dockerfile, err)
			}
		}
		for _, k := range sortedKeys(b.Args) {
			if !envKeyRE.MatchString(k) {
				v.errorf("build.arg.invalid", base+".build.args."+k, "", "invalid build arg name %q", k)
			}
		}
	}

	if image != "" && v.policy.AllowedRegistries != nil && imageRE.MatchString(image) &&
		!hasAnyPrefix(image, v.policy.AllowedRegistries) {
		v.errorf("policy.image_denied", base+".image",
			"Build from the repository with `build:`, or ask an admin to allow this registry.",
			"image %q is not from a registry allowed by your organisation's policy", image)
	}
	if image != "" && !imageRE.MatchString(image) {
		hint := "Use a registry reference such as `postgres:16` or `ghcr.io/org/app:tag`."
		if strings.HasPrefix(image, ".") || strings.HasPrefix(image, "/") {
			hint = "That looks like a path. To build from the repo, use `build: {context: ...}`."
		}
		v.errorf("image.invalid", base+".image", hint, "invalid image reference %q", image)
	}
}

func (v *validator) port(base string, port int) {
	switch {
	case port == 0:
		v.errorf("port.missing", base, "Set the port your process listens on, e.g. `port: 8080`.",
			"%s needs a port", base)
	case port < 1 || port > 65535:
		v.errorf("port.invalid", base+".port", "", "port %d is out of range (1-65535)", port)
	}
}

func (v *validator) health(base string, h *Health) {
	if h == nil {
		return
	}
	if h.Path == "" || !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, " \t\r\n") {
		v.errorf("health.path.invalid", base+".path", "Example: `path: /health`.",
			"health path %q must start with '/' and contain no spaces", h.Path)
	}
	if h.InitialDelay.Std().Seconds() > maxHealthDelaySecs {
		v.errorf("health.delay.out_of_range", base+".initialDelay", "",
			"initialDelay must be at most %ds", maxHealthDelaySecs)
	}
}

func (v *validator) env(base string, env map[string]string) {
	for _, k := range sortedKeys(env) {
		p := base + ".env." + k
		switch {
		case !envKeyRE.MatchString(k):
			v.errorf("env.name.invalid", p, "", "invalid environment variable name %q", k)
		case strings.HasPrefix(strings.ToUpper(k), "HEIMDALL_"):
			v.errorf("env.reserved", p, "The HEIMDALL_ prefix is reserved for variables injected by the platform.",
				"environment variable %q uses a reserved prefix", k)
		}
		if env[k] != "" && secretLikeRE.MatchString(k) {
			v.warnf("env.secret_literal", p,
				"Move it to `secrets:` so the value is stored encrypted and never committed.",
				"%q looks like a secret but has a literal value in the repo", k)
		}
	}
}

func (v *validator) secrets(base string, secrets []string, env map[string]string) {
	seen := map[string]bool{}
	for i, s := range secrets {
		p := fmt.Sprintf("%s.secrets[%d]", base, i)
		switch {
		case !secretRE.MatchString(s):
			v.errorf("secret.invalid", p, "Use UPPER_SNAKE_CASE, e.g. STRIPE_TEST_KEY.", "invalid secret name %q", s)
		case seen[s]:
			v.errorf("secret.duplicate", p, "", "secret %q is listed twice", s)
		}
		if _, clash := env[s]; clash {
			v.errorf("secret.conflict", p, "", "%q is defined in both env and secrets", s)
		}
		if v.policy.AllowedSecrets != nil && !slices.Contains(v.policy.AllowedSecrets, s) {
			v.errorf("policy.secret_denied", p,
				"Secrets must be approved by an admin before previews can use them.",
				"secret %q is not allowed by your organisation's policy", s)
		}
		seen[s] = true
	}
}

// injected rejects env vars and secrets that would shadow a variable Heimdall
// injects into this workload.
func (v *validator) injected(base string, service bool, env map[string]string, secrets []string) {
	reserved := map[string]string{}
	if service {
		reserved[EnvPort] = "PORT is set to the service's port."
	}
	for dep, name := range map[string]string{DepPostgres: EnvDatabaseURL, DepRedis: EnvRedisURL, DepRabbitMQ: EnvAMQPURL} {
		if v.cfg.DependencyEnabled(dep) {
			reserved[name] = fmt.Sprintf("%s points at the environment's own %s.", name, dep)
		}
	}
	for _, k := range sortedKeys(env) {
		if why, ok := reserved[k]; ok {
			v.errorf("env.reserved", base+".env."+k, why+" Remove it from env.", "%q is injected by Heimdall", k)
		}
	}
	for i, s := range secrets {
		if why, ok := reserved[s]; ok {
			v.errorf("env.reserved", fmt.Sprintf("%s.secrets[%d]", base, i), why+" Remove it from secrets.",
				"%q is injected by Heimdall", s)
		}
	}
}

func (v *validator) resources(base string, r Resources) {
	if r.Size != "" {
		_, known := sizePresets[r.Size]
		switch {
		case !known:
			v.errorf("resources.size.invalid", base+".size", "Use one of: small, medium, large.",
				"invalid size %q", r.Size)
		case r.CPU != "" || r.Memory != "" || r.Requests != (Requests{}):
			v.errorf("resources.size.conflict", base+".size",
				"A size sets cpu and memory for you; remove either `size` or the explicit values.",
				"size %q cannot be combined with cpu, memory or requests", r.Size)
		case r.Size == SizeLarge && !v.policy.AllowLargeSize:
			v.errorf("policy.size_denied", base+".size",
				"Use `medium`, or ask an admin to approve `large` for your organisation.",
				"size %q is not allowed by your organisation's policy", r.Size)
		}
	}
	v.cpu(base+".cpu", r.CPU, true)
	v.memory(base+".memory", r.Memory, true)
	v.cpu(base+".requests.cpu", r.Requests.CPU, false)
	v.memory(base+".requests.memory", r.Requests.Memory, false)
}

// cpu and memory validate one quantity. Limits are also checked against the
// per-container ceiling; requests are bounded by their limit later, in
// requests(), once defaults are known.
func (v *validator) cpu(p, s string, limit bool) {
	if s == "" {
		return
	}
	m, err := parseCPU(s)
	switch {
	case err != nil:
		v.errorf("resources.invalid", p, err.Error(), "invalid cpu %q", s)
	case limit && m > v.policy.MaxContainerCPUMilli:
		v.errorf("resources.too_large", p, "", "cpu %q exceeds the per-container limit of %dm", s, v.policy.MaxContainerCPUMilli)
	}
}

func (v *validator) memory(p, s string, limit bool) {
	if s == "" {
		return
	}
	m, err := parseMebibytes(s)
	switch {
	case err != nil:
		v.errorf("resources.invalid", p, err.Error(), "invalid memory %q", s)
	case limit && m > v.policy.MaxContainerMemoryMi:
		v.errorf("resources.too_large", p, "", "memory %q exceeds the per-container limit of %dMi", s, v.policy.MaxContainerMemoryMi)
	}
}

// requests runs after defaulting, when every workload has limits and
// requests, and checks that they agree.
func (v *validator) requests() {
	for _, n := range sortedKeys(v.cfg.Services) {
		v.checkRequests("services."+n+".resources", v.cfg.Services[n].Resources)
	}
	for _, n := range sortedKeys(v.cfg.Workers) {
		v.checkRequests("workers."+n+".resources", v.cfg.Workers[n].Resources)
	}
}

func (v *validator) checkRequests(base string, r Resources) {
	if req, lim := milli(r.Requests.CPU), milli(r.CPU); req > lim {
		v.errorf("resources.requests.exceeds_limit", base+".requests.cpu", "A request can be at most its limit.",
			"cpu request %s exceeds the cpu limit %s", r.Requests.CPU, r.CPU)
	}
	req, lim := mebi(r.Requests.Memory), mebi(r.Memory)
	pct := v.policy.MinMemoryRequestPercent
	switch {
	case req > lim:
		v.errorf("resources.requests.exceeds_limit", base+".requests.memory", "A request can be at most its limit.",
			"memory request %s exceeds the memory limit %s", r.Requests.Memory, r.Memory)
	case pct > 0 && req*100 < lim*pct:
		v.errorf("resources.requests.too_low", base+".requests.memory",
			fmt.Sprintf("Request at least %dMi, or lower the memory limit. Low memory requests let nodes be over-packed until previews are OOM-killed.",
				ceilDiv(lim*pct, 100)),
			"memory request %s is below %d%% of the memory limit %s", r.Requests.Memory, pct, r.Memory)
	}
}

func (v *validator) dependencies() {
	d := v.cfg.Dependencies
	if p := d.Postgres; p != nil {
		if p.Version != "" && !slices.Contains(supportedPostgres, p.Version) {
			v.errorf("postgres.version.unsupported", "dependencies.postgres.version",
				"Supported: "+strings.Join(supportedPostgres, ", ")+".", "unsupported PostgreSQL version %q", p.Version)
		}
		if p.Storage != "" {
			mi, err := parseMebibytes(p.Storage)
			switch {
			case err != nil:
				v.errorf("resources.invalid", "dependencies.postgres.storage", err.Error(), "invalid storage %q", p.Storage)
			case mi > v.policy.MaxPostgresStorageMi:
				v.errorf("resources.too_large", "dependencies.postgres.storage", "",
					"storage %q exceeds the database size limit of %dMi", p.Storage, v.policy.MaxPostgresStorageMi)
			}
		}
		if p.Seed != "" {
			if err := checkRelPath(p.Seed); err != nil {
				v.errorf("path.invalid", "dependencies.postgres.seed", "", "seed file %q %v", p.Seed, err)
			} else if !strings.HasSuffix(strings.ToLower(p.Seed), ".sql") {
				v.errorf("seed.extension", "dependencies.postgres.seed", "Seed files must be plain .sql.",
					"seed file %q must end in .sql", p.Seed)
			}
		}
	}
	if r := d.Redis; r != nil && r.Version != "" && !slices.Contains(supportedRedis, r.Version) {
		v.errorf("redis.version.unsupported", "dependencies.redis.version",
			"Supported: "+strings.Join(supportedRedis, ", ")+".", "unsupported Redis version %q", r.Version)
	}
	if r := d.RabbitMQ; r != nil && r.Version != "" && !slices.Contains(supportedRabbitMQ, r.Version) {
		v.errorf("rabbitmq.version.unsupported", "dependencies.rabbitmq.version",
			"Supported: "+strings.Join(supportedRabbitMQ, ", ")+".", "unsupported RabbitMQ version %q", r.Version)
	}
}

func (v *validator) migrations() {
	m := v.cfg.Migrations
	if m == nil {
		return
	}
	if v.cfg.Dependencies.Postgres == nil {
		v.errorf("migrations.requires_postgres", "migrations", "Enable `dependencies.postgres`.",
			"migrations are configured but no database is enabled")
	}
	switch _, ok := v.cfg.Services[m.Service]; {
	case m.Service == "":
		v.errorf("migrations.service.missing", "migrations",
			"Name the service whose image contains your migration tool.", "migrations.service is required")
	case !ok:
		v.errorf("migrations.service.unknown", "migrations.service",
			"It must match a key under `services`.", "migrations.service %q is not a defined service", m.Service)
	}
	if strings.TrimSpace(m.Command) == "" {
		v.errorf("migrations.command.missing", "migrations", "Example: `command: npm run migrate`.",
			"migrations.command is required")
	}
	if m.Timeout.Std().Minutes() > maxMigrationTime {
		v.errorf("migrations.timeout.out_of_range", "migrations.timeout", "",
			"migration timeout must be at most %dm", maxMigrationTime)
	}
}

func (v *validator) smokeTests() {
	if len(v.cfg.SmokeTests) > maxSmokeTests {
		v.errorf("smoke.too_many", "smokeTests", "", "%d smoke tests declared; the limit is %d",
			len(v.cfg.SmokeTests), maxSmokeTests)
	}
	seen := map[string]bool{}
	for i, t := range v.cfg.SmokeTests {
		base := fmt.Sprintf("smokeTests[%d]", i)
		switch {
		case t.Name == "":
			v.errorf("smoke.name.missing", base, "Give each test a short name for the PR report.", "smoke test needs a name")
		case !nameRE.MatchString(t.Name):
			v.errorf("name.invalid", base+".name",
				"Use lowercase letters, digits and '-'.", "invalid smoke test name %q", t.Name)
		case seen[t.Name]:
			v.errorf("name.duplicate", base+".name", "", "smoke test name %q is used twice", t.Name)
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Command) == "" {
			v.errorf("smoke.command.missing", base, "Example: `command: curl -f http://api:8080/health`.",
				"smoke test %q needs a command", t.Name)
		}
		if t.Timeout.Std().Minutes() > maxSmokeTestTime {
			v.errorf("smoke.timeout.out_of_range", base+".timeout", "",
				"smoke test timeout must be at most %dm", maxSmokeTestTime)
		}
	}
}

func (v *validator) preview() {
	p := v.cfg.Preview
	ttl := p.TTL.Std()
	if ttl != 0 && (ttl < v.policy.MinTTL || ttl > v.policy.MaxTTL) {
		v.errorf("preview.ttl.out_of_range", "preview.ttl", "",
			"ttl %s must be between %s and %s", p.TTL, Duration(v.policy.MinTTL), Duration(v.policy.MaxTTL))
	}
	if s := p.SleepAfter.Std(); s != 0 {
		effective := ttl
		if effective == 0 {
			effective = defaultTTL
		}
		switch {
		case s.Minutes() < minSleepAfterMin:
			v.errorf("preview.sleepAfter.out_of_range", "preview.sleepAfter", "",
				"sleepAfter must be at least %dm", minSleepAfterMin)
		case s >= effective:
			v.errorf("preview.sleepAfter.out_of_range", "preview.sleepAfter",
				"Sleeping only makes sense before the environment expires.",
				"sleepAfter (%s) must be shorter than ttl (%s)", p.SleepAfter, Duration(effective))
		}
	}
	if visibilityRank(p.Visibility) > visibilityRank(v.policy.MaxVisibility) && visibilityRank(p.Visibility) < 3 {
		v.errorf("policy.visibility_denied", "preview.visibility",
			fmt.Sprintf("Your organisation allows at most %q.", v.policy.MaxVisibility),
			"visibility %q is more open than policy allows", p.Visibility)
	}
	switch p.Visibility {
	case "", VisibilityPrivate, VisibilityOrg:
	case VisibilityPublic:
		v.warnf("preview.visibility.public", "preview.visibility",
			"Use `org` or `private` unless reviewers outside your organisation need access.",
			"anyone with the URL will be able to open this preview")
	default:
		v.errorf("preview.visibility.invalid", "preview.visibility", "Use one of: private, org, public.",
			"invalid visibility %q", p.Visibility)
	}
}

// graph validates dependsOn references and rejects cycles. Only services can be
// depended upon (workers have no network identity), so cycles can only form
// between services.
func (v *validator) graph() {
	edges := map[string][]string{}
	check := func(kind, name string, deps []string) {
		base := fmt.Sprintf("%s.%s.dependsOn", kind, name)
		seen := map[string]bool{}
		for i, d := range deps {
			p := fmt.Sprintf("%s[%d]", base, i)
			_, isService := v.cfg.Services[d]
			_, isWorker := v.cfg.Workers[d]
			switch {
			case d == name && kind == "services":
				v.errorf("dependsOn.self", p, "", "%s cannot depend on itself", name)
			case seen[d]:
				v.warnf("dependsOn.duplicate", p, "", "%q is listed twice", d)
			case isDependencyName(d):
				if !v.cfg.DependencyEnabled(d) {
					v.errorf("dependsOn.unknown", p,
						fmt.Sprintf("Enable it under `dependencies: {%s: {}}`.", d),
						"%q is not enabled", d)
				}
			case isWorker:
				v.errorf("dependsOn.worker", p, "Workers cannot be depended upon; depend on a service or dependency.",
					"%q is a worker", d)
			case !isService:
				v.errorf("dependsOn.unknown", p, "", "%q is not a defined service or dependency", d)
			case kind == "services":
				edges[name] = append(edges[name], d)
			}
			seen[d] = true
		}
	}
	for _, n := range sortedKeys(v.cfg.Services) {
		check("services", n, v.cfg.Services[n].DependsOn)
	}
	for _, n := range sortedKeys(v.cfg.Workers) {
		check("workers", n, v.cfg.Workers[n].DependsOn)
	}
	if cycle := findCycle(edges); cycle != nil {
		v.errorf("dependsOn.cycle", "services."+cycle[0]+".dependsOn", "Break the loop by removing one of these edges.",
			"dependency cycle: %s", strings.Join(cycle, " -> "))
	}
}

// totals runs after defaulting and checks the whole environment against the
// tenant's ceilings, including the fixed cost of managed dependencies.
func (v *validator) totals() {
	cpu, mem := 0, 0
	for _, s := range v.cfg.Services {
		c, _ := parseCPU(s.Resources.CPU)
		m, _ := parseMebibytes(s.Resources.Memory)
		cpu, mem = cpu+c, mem+m
	}
	for _, w := range v.cfg.Workers {
		c, _ := parseCPU(w.Resources.CPU)
		m, _ := parseMebibytes(w.Resources.Memory)
		cpu, mem = cpu+c*w.Replicas, mem+m*w.Replicas
	}
	for _, d := range v.cfg.EnabledDependencies() {
		cpu += dependencyCost[d].cpuMilli
		mem += dependencyCost[d].memoryMi
	}
	hint := "Lower resources.cpu/memory, reduce worker replicas, or drop optional dependencies."
	if cpu > v.policy.MaxTotalCPUMilli {
		v.errorf("quota.cpu", "", hint, "environment needs %dm CPU in total; the limit is %dm", cpu, v.policy.MaxTotalCPUMilli)
	}
	if mem > v.policy.MaxTotalMemoryMi {
		v.errorf("quota.memory", "", hint, "environment needs %dMi memory in total; the limit is %dMi", mem, v.policy.MaxTotalMemoryMi)
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func isDependencyName(s string) bool {
	return s == DepPostgres || s == DepRedis || s == DepRabbitMQ
}

// checkRelPath ensures p is a clean, forward-slash path that stays inside the
// repository. Config comes from the PR author; never trust it as a filesystem
// path without this check.
func checkRelPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("must not be empty")
	case strings.ContainsRune(p, '\\'):
		return fmt.Errorf("must use forward slashes")
	case strings.HasPrefix(p, "/") || driveRE.MatchString(p):
		return fmt.Errorf("must be relative to the repository root")
	}
	if c := path.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("must stay inside the repository")
	}
	return nil
}

// findCycle returns the first dependency cycle (as a closed path such as
// [a b a]) or nil. Iteration order is sorted so output is deterministic.
func findCycle(graph map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack, cycle []string
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		for _, m := range graph[n] {
			switch color[m] {
			case grey:
				cycle = append(slices.Clone(stack[slices.Index(stack, m):]), m)
				return true
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range sortedKeys(graph) {
		if color[n] == white && visit(n) {
			return cycle
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
