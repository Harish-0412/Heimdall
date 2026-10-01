package render

import (
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// Secrets referenced by pods.
const (
	// CredentialsSecret holds generated per-environment passwords and the
	// connection URLs built from them. Rendered when Context.Credentials is
	// set; otherwise the caller manages it.
	CredentialsSecret = "heimdall-credentials"
	// AppSecretsSecret holds the values of the secrets workloads list in
	// heimdall.yaml, one key per name. Delivered by the platform (External
	// Secrets in P7), never rendered.
	AppSecretsSecret = "heimdall-app-secrets"
)

// Keys of CredentialsSecret.
const (
	keyPostgresSuperuserPassword = "postgres-superuser-password"
	keyPostgresAppPassword       = "postgres-app-password"
	keyDatabaseURL               = "database-url"
	keyDatabaseURLBaseline       = "database-url-baseline"
	keyRedisPassword             = "redis-password"
	keyRedisURL                  = "redis-url"
	keyRabbitMQPassword          = "rabbitmq-password"
	keyAMQPURL                   = "amqp-url"
)

// Variables Heimdall injects, besides config.EnvPort and the dependency URLs.
const (
	EnvPR              = "HEIMDALL_PR"
	EnvSHA             = "HEIMDALL_SHA"
	EnvPublicURL       = "HEIMDALL_PUBLIC_URL"  // the primary service's URL
	EnvPublicURLPrefix = "HEIMDALL_PUBLIC_URL_" // + service name, upper snake case
)

// database selects which database DATABASE_URL points at.
type database int

const (
	liveDatabase database = iota
	baselineDatabase
)

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret},
		Key:                  key,
	}}}
}

func credential(name, key string) corev1.EnvVar { return secretEnv(name, CredentialsSecret, key) }

// platformEnv is what every workload and test can rely on: which PR and commit
// is running and where it is reachable. Frontends read the URLs at runtime, so
// one image works in every preview.
func (b *builder) platformEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: EnvPR, Value: strconv.Itoa(b.ctx.PR)},
		{Name: EnvSHA, Value: b.ctx.SHA},
	}
	if primary := b.cfg.PrimaryService(); primary != "" {
		env = append(env, corev1.EnvVar{Name: EnvPublicURL, Value: b.urls[primary]})
	}
	for _, name := range sortedKeys(b.urls) {
		env = append(env, corev1.EnvVar{Name: EnvPublicURLPrefix + envVarSuffix(name), Value: b.urls[name]})
	}
	return env
}

// dependencyEnv points workloads at the environment's own backing services.
// Connection URLs come from CredentialsSecret, so no password is ever written
// into a pod spec.
func (b *builder) dependencyEnv(db database) []corev1.EnvVar {
	var env []corev1.EnvVar
	if b.cfg.DependencyEnabled(config.DepPostgres) {
		key := keyDatabaseURL
		if db == baselineDatabase {
			key = keyDatabaseURLBaseline
		}
		env = append(env, credential(config.EnvDatabaseURL, key))
	}
	if b.cfg.DependencyEnabled(config.DepRedis) {
		env = append(env, credential(config.EnvRedisURL, keyRedisURL))
	}
	if b.cfg.DependencyEnabled(config.DepRabbitMQ) {
		env = append(env, credential(config.EnvAMQPURL, keyAMQPURL))
	}
	return env
}

// workloadEnv assembles a workload container's environment in a fixed order:
// PORT, platform variables, dependency URLs, the user's env (sorted), then the
// user's secrets (sorted). Duplicate names are an error rather than a silent
// override; Load already rejects them, so this guards the renderer itself.
func (b *builder) workloadEnv(field string, port int, user map[string]string, secrets []string, db database) []corev1.EnvVar {
	var env []corev1.EnvVar
	if port > 0 {
		env = append(env, corev1.EnvVar{Name: config.EnvPort, Value: strconv.Itoa(port)})
	}
	env = append(env, b.platformEnv()...)
	env = append(env, b.dependencyEnv(db)...)
	for _, k := range sortedKeys(user) {
		env = append(env, corev1.EnvVar{Name: k, Value: user[k]})
	}
	for _, s := range slices.Sorted(slices.Values(secrets)) {
		env = append(env, secretEnv(s, AppSecretsSecret, s))
	}
	seen := map[string]bool{}
	for _, e := range env {
		if seen[e.Name] {
			b.errs.add(CodeEnvConflict, field, "environment variable %s is defined twice", e.Name)
		}
		seen[e.Name] = true
	}
	return env
}
