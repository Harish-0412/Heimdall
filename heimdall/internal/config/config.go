// Package config defines the heimdall.yaml schema and everything needed to
// load it safely: strict parsing, source-located diagnostics, semantic
// validation and defaulting.
//
// The Go types in this file are the single source of truth for the schema.
// The CLI, the API (on webhook receipt) and the in-cluster controller all load
// configuration through [Load], so a file that validates locally behaves the
// same everywhere.
package config

import "slices"

// CurrentVersion is the only schema version understood today. Bump it (and add
// a migration) for breaking changes; additive changes do not need a bump.
const CurrentVersion = 1

// Names of the managed backing services Heimdall can provision. They are also
// reserved DNS names inside every preview namespace.
const (
	DepPostgres = "postgres"
	DepRabbitMQ = "rabbitmq"
	DepRedis    = "redis"
)

// Visibility values for Preview.Visibility.
const (
	VisibilityPrivate = "private" // only the PR author and repo collaborators
	VisibilityOrg     = "org"     // any member of the GitHub organisation
	VisibilityPublic  = "public"  // anyone with the URL
)

// Size presets for Resources.Size. Services default to medium and workers to
// small; large needs approval in the tenant Policy.
const (
	SizeSmall  = "small"
	SizeMedium = "medium"
	SizeLarge  = "large"
)

// Environment variables Heimdall injects into workloads. Workloads may not
// set them through env or secrets: two definitions of one variable would make
// the effective value depend on ordering. Names with the HEIMDALL_ prefix are
// reserved as a whole.
const (
	EnvPort        = "PORT"         // services only: the declared port
	EnvDatabaseURL = "DATABASE_URL" // when postgres is enabled
	EnvRedisURL    = "REDIS_URL"    // when redis is enabled
	EnvAMQPURL     = "AMQP_URL"     // when rabbitmq is enabled
)

// Config is the root of heimdall.yaml.
type Config struct {
	// Version of the schema. Required.
	Version int `yaml:"version"`
	// Services are long-running network workloads. At least one is required.
	Services map[string]Service `yaml:"services"`
	// Dependencies are managed backing services (database, cache, broker).
	Dependencies Dependencies `yaml:"dependencies"`
	// Workers are long-running workloads without a network port.
	Workers map[string]Worker `yaml:"workers"`
	// Migrations describes the schema-migration step run before services start.
	Migrations *Migrations `yaml:"migrations"`
	// SmokeTests run inside the namespace once everything is healthy.
	SmokeTests []SmokeTest `yaml:"smokeTests"`
	// Preview controls lifecycle and access.
	Preview Preview `yaml:"preview"`

	// src records where each key came from, so later checks (for example
	// CompareToBaseline) can still point at lines. Set by Load.
	src *SourceMap
}

// Build describes how to build an image from the repository.
type Build struct {
	// Context is the Docker build context, relative to the repository root.
	// Defaults to ".".
	Context string `yaml:"context"`
	// Dockerfile is the path to the Dockerfile, relative to Context.
	// Defaults to "Dockerfile".
	Dockerfile string `yaml:"dockerfile"`
	// Args are build arguments. Never put secrets here: they end up in image
	// history.
	Args map[string]string `yaml:"args"`
}

// Health configures the HTTP readiness probe. Without it Heimdall falls back
// to a TCP probe on the service port.
type Health struct {
	Path         string   `yaml:"path"`
	InitialDelay Duration `yaml:"initialDelay"`
}

// Resources sizes one container: either a Size preset, or explicit limits
// (CPU, Memory) with optional Requests. After Load, CPU, Memory and both
// requests are always set, whichever form was used.
type Resources struct {
	// Size is a preset: small, medium or large. It sets the limits; it cannot
	// be combined with cpu, memory or requests.
	Size string `yaml:"size"`
	// CPU limit as cores ("1", "0.5") or millicores ("250m").
	CPU string `yaml:"cpu"`
	// Memory limit in binary units: "256Mi" or "1Gi".
	Memory string `yaml:"memory"`
	// Requests is what the scheduler reserves for the container. Defaults:
	// CPU 20% of its limit (it may burst), memory 50% of its limit and at
	// least 64Mi (close to the working set, so nodes are not over-packed).
	Requests Requests `yaml:"requests"`
}

// Requests are scheduler reservations. Each must not exceed its limit, and
// the memory request may not be lower than the policy's minimum share of the
// memory limit (half, by default).
type Requests struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// Service is a long-running workload that listens on a port.
type Service struct {
	// Build or Image: exactly one must be set.
	Build *Build `yaml:"build"`
	Image string `yaml:"image"`
	// Port the process listens on. Required.
	Port int `yaml:"port"`
	// Public exposes the service through a preview URL.
	Public bool `yaml:"public"`
	// Primary marks the service that owns the bare preview URL. Optional when
	// exactly one service is public; required when several are.
	Primary bool `yaml:"primary"`
	// Env are plain, non-secret environment variables.
	Env map[string]string `yaml:"env"`
	// Secrets lists names of secrets (stored in Heimdall) to inject as env vars.
	Secrets   []string  `yaml:"secrets"`
	Health    *Health   `yaml:"health"`
	Resources Resources `yaml:"resources"`
	// DependsOn lists services and enabled dependencies that must be ready first.
	DependsOn []string `yaml:"dependsOn"`
}

// Worker is a long-running workload without a port (queue consumers, cron-like
// loops).
type Worker struct {
	Build *Build `yaml:"build"`
	Image string `yaml:"image"`
	// Command is run through "sh -c". Required.
	Command string            `yaml:"command"`
	Env     map[string]string `yaml:"env"`
	Secrets []string          `yaml:"secrets"`
	// Replicas defaults to 1; the maximum is 5.
	Replicas  int       `yaml:"replicas"`
	Resources Resources `yaml:"resources"`
	DependsOn []string  `yaml:"dependsOn"`
}

// Dependencies lists the managed backing services. A dependency is enabled by
// its presence; use "{}" to enable it with defaults.
type Dependencies struct {
	Postgres *Postgres `yaml:"postgres"`
	Redis    *Redis    `yaml:"redis"`
	RabbitMQ *RabbitMQ `yaml:"rabbitmq"`
}

// Postgres configures the per-environment PostgreSQL instance.
type Postgres struct {
	// Version is the major version. Defaults to 16.
	Version string `yaml:"version"`
	// Storage caps the database volume (for example "2Gi"). Defaults to 1Gi.
	Storage string `yaml:"storage"`
	// Seed is a .sql file, relative to the repository root, applied after
	// migrations. It is also the baseline restored by "reset".
	Seed string `yaml:"seed"`
}

// Redis configures the per-environment Redis instance.
type Redis struct {
	Version string `yaml:"version"`
}

// RabbitMQ configures the per-environment RabbitMQ broker.
type RabbitMQ struct {
	Version string `yaml:"version"`
}

// Migrations is the schema-migration step. It runs as a Job from the image of
// Service, after Postgres is ready and before any service starts.
type Migrations struct {
	Service string `yaml:"service"`
	// Command is run through "sh -c".
	Command string `yaml:"command"`
	// Timeout defaults to 5m.
	Timeout Duration `yaml:"timeout"`
}

// SmokeTest is a command run in a Job inside the preview namespace, so service
// names resolve as hostnames (for example "curl -f http://api:8080/health").
type SmokeTest struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	// Timeout defaults to 2m.
	Timeout Duration `yaml:"timeout"`
}

// Preview controls the environment lifecycle and who may open it.
type Preview struct {
	// TTL is how long the environment lives. Defaults to 48h.
	TTL Duration `yaml:"ttl"`
	// SleepAfter scales the environment to zero after this much inactivity.
	// Disabled when unset.
	SleepAfter Duration `yaml:"sleepAfter"`
	// Visibility defaults to "private".
	Visibility string `yaml:"visibility"`
}

// EnabledDependencies returns the names of enabled dependencies, sorted.
func (c *Config) EnabledDependencies() []string {
	var out []string
	if c.Dependencies.Postgres != nil {
		out = append(out, DepPostgres)
	}
	if c.Dependencies.RabbitMQ != nil {
		out = append(out, DepRabbitMQ)
	}
	if c.Dependencies.Redis != nil {
		out = append(out, DepRedis)
	}
	return out
}

// DependencyEnabled reports whether the named dependency is enabled.
func (c *Config) DependencyEnabled(name string) bool {
	return slices.Contains(c.EnabledDependencies(), name)
}

// PrimaryService returns the service that owns the bare preview URL, or "" if
// no service is public. Only meaningful on a normalised (loaded) Config.
func (c *Config) PrimaryService() string {
	for name, s := range c.Services {
		if s.Primary {
			return name
		}
	}
	return ""
}

// Loaded reports whether c was produced by [Load], and is therefore validated
// and has every default applied. Consumers such as the renderer refuse
// hand-built configs, which would skip validation.
func (c *Config) Loaded() bool { return c != nil && c.src != nil }
