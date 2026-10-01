package config

import (
	"fmt"
	"slices"
	"time"
)

// Defaults applied after successful validation. Keeping them in one place
// makes "what happens if I omit X?" answerable by reading a single file.
const (
	defaultServiceCPU    = "500m" // the medium preset
	defaultServiceMemory = "512Mi"
	defaultWorkerCPU     = "250m" // the small preset
	defaultWorkerMemory  = "256Mi"

	defaultBuildContext = "."
	defaultDockerfile   = "Dockerfile"

	defaultPostgresVersion = "16"
	defaultPostgresStorage = "1Gi"
	defaultRedisVersion    = "7"
	defaultRabbitVersion   = "3.13"

	defaultTTL              = 48 * time.Hour
	defaultVisibility       = VisibilityPrivate
	defaultMigrationTimeout = 5 * time.Minute
	defaultSmokeTimeout     = 2 * time.Minute
	defaultHealthDelay      = 5 * time.Second
)

// Request defaults (ADR 0007): CPU may burst, so its request is low; memory is
// not compressible, so its request stays close to the expected working set.
const (
	cpuRequestPercent           = 20
	minCPURequestMilli          = 10
	defaultMemoryRequestPercent = 50
	minMemoryRequestMi          = 64
)

// sizePresets map a Size to container limits. Requests are derived from the
// limits like any other. small and medium equal the worker and service
// defaults, so omitting resources is the same as choosing the golden path.
var sizePresets = map[string]struct{ cpu, memory string }{
	SizeSmall:  {defaultWorkerCPU, defaultWorkerMemory},
	SizeMedium: {defaultServiceCPU, defaultServiceMemory},
	SizeLarge:  {"1", "2Gi"},
}

// Versions Heimdall can provision. The renderer pins an image digest for each
// of them (internal/render/catalog.go); a test keeps the two lists in step.
var (
	supportedPostgres = []string{"14", "15", "16", "17"}
	supportedRedis    = []string{"6", "7"}
	supportedRabbitMQ = []string{"3.12", "3.13"}
)

// dependencyCost is the fixed resource limit of each managed dependency,
// counted against the environment totals.
var dependencyCost = map[string]struct{ cpuMilli, memoryMi int }{
	DepPostgres: {500, 512},
	DepRedis:    {100, 128},
	DepRabbitMQ: {250, 512},
}

// SupportedVersions returns the versions Heimdall can provision for the named
// managed dependency, oldest first, or nil for an unknown name.
func SupportedVersions(dep string) []string {
	switch dep {
	case DepPostgres:
		return slices.Clone(supportedPostgres)
	case DepRedis:
		return slices.Clone(supportedRedis)
	case DepRabbitMQ:
		return slices.Clone(supportedRabbitMQ)
	}
	return nil
}

// DependencyResources returns the container resources of a managed dependency
// (limits and derived requests): the same reservation the quota check counts.
// It panics on an unknown name, which is a programming error.
func DependencyResources(dep string, p Policy) Resources {
	c, ok := dependencyCost[dep]
	if !ok {
		panic(fmt.Sprintf("config: unknown dependency %q", dep))
	}
	return ResolveResources(Resources{
		CPU:    fmt.Sprintf("%dm", c.cpuMilli),
		Memory: fmt.Sprintf("%dMi", c.memoryMi),
	}, SizeSmall, p)
}

// ResolveResources returns r with limits and requests filled in exactly as
// Load fills them for workloads: a Size preset becomes limits, missing limits
// come from the fallback preset, and missing requests are derived from the
// limits under p. r must already be valid; fallback must be a preset name.
func ResolveResources(r Resources, fallback string, p Policy) Resources {
	if preset, ok := sizePresets[r.Size]; ok {
		r.CPU, r.Memory = preset.cpu, preset.memory
	}
	def := sizePresets[fallback]
	r.CPU = orDefault(r.CPU, def.cpu)
	r.Memory = orDefault(r.Memory, def.memory)
	if r.Requests.CPU == "" {
		r.Requests.CPU = fmt.Sprintf("%dm", defaultCPURequest(milli(r.CPU)))
	}
	if r.Requests.Memory == "" {
		pct := max(p.MinMemoryRequestPercent, defaultMemoryRequestPercent)
		r.Requests.Memory = fmt.Sprintf("%dMi", defaultMemoryRequest(mebi(r.Memory), pct))
	}
	return r
}

func defaultCPURequest(limitMilli int) int {
	return min(max(limitMilli*cpuRequestPercent/100, minCPURequestMilli), limitMilli)
}

// defaultMemoryRequest rounds up, so limit/request never exceeds 100/percent
// (a LimitRange with that maxLimitRequestRatio then admits it).
func defaultMemoryRequest(limitMi, percent int) int {
	return min(max(ceilDiv(limitMi*percent, 100), minMemoryRequestMi), limitMi)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

func (c *Config) applyDefaults(p Policy) {
	for name, s := range c.Services {
		applyBuildDefaults(s.Build)
		s.Resources = ResolveResources(s.Resources, SizeMedium, p)
		if s.Health != nil && s.Health.InitialDelay == 0 {
			s.Health.InitialDelay = Duration(defaultHealthDelay)
		}
		c.Services[name] = s
	}
	c.defaultPrimary()

	for name, w := range c.Workers {
		applyBuildDefaults(w.Build)
		if w.Replicas == 0 {
			w.Replicas = 1
		}
		w.Resources = ResolveResources(w.Resources, SizeSmall, p)
		c.Workers[name] = w
	}

	if p := c.Dependencies.Postgres; p != nil {
		p.Version = orDefault(p.Version, defaultPostgresVersion)
		p.Storage = orDefault(p.Storage, defaultPostgresStorage)
	}
	if r := c.Dependencies.Redis; r != nil {
		r.Version = orDefault(r.Version, defaultRedisVersion)
	}
	if r := c.Dependencies.RabbitMQ; r != nil {
		r.Version = orDefault(r.Version, defaultRabbitVersion)
	}

	if m := c.Migrations; m != nil && m.Timeout == 0 {
		m.Timeout = Duration(defaultMigrationTimeout)
	}
	for i := range c.SmokeTests {
		if c.SmokeTests[i].Timeout == 0 {
			c.SmokeTests[i].Timeout = Duration(defaultSmokeTimeout)
		}
	}

	if c.Preview.TTL == 0 {
		c.Preview.TTL = Duration(defaultTTL)
	}
	c.Preview.Visibility = orDefault(c.Preview.Visibility, defaultVisibility)
}

// defaultPrimary makes the implicit primary explicit: with exactly one public
// service, that service is the primary one.
func (c *Config) defaultPrimary() {
	var public []string
	for name, s := range c.Services {
		if s.Public {
			public = append(public, name)
		}
	}
	if len(public) == 1 {
		s := c.Services[public[0]]
		s.Primary = true
		c.Services[public[0]] = s
	}
}

func applyBuildDefaults(b *Build) {
	if b == nil {
		return
	}
	b.Context = orDefault(b.Context, defaultBuildContext)
	b.Dockerfile = orDefault(b.Dockerfile, defaultDockerfile)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
