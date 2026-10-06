package render

import (
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// The baseline-database stage (ADR 0007, P2's data lifecycle):
//
//	prepare  fresh, empty baseline database owned by the app role
//	migrate  the PR's migrations, against the baseline
//	seed     an operator-approved sanitised import, against the baseline
//	clone    freeze the baseline as a template; clone the live database from it
//
// Reset later repeats only the clone script, which is why it is a separate
// file. Each generation rebuilds the baseline from scratch, so a preview always
// reflects exactly its commit's migrations and seed.
//
// Trust boundary: migrations and seed are PR-controlled, so they run as the
// unprivileged app role (no superuser, so no COPY ... PROGRAM or file
// access). Only Heimdall's own scripts below run as the superuser.

const (
	dbScriptsConfigMap = "heimdall-db-scripts"
	scriptsDir         = "/scripts"
	seedDir            = "/seed"
	seedKey            = "seed.sql"

	dbAdminTimeout = 5 * time.Minute
	seedTimeout    = 10 * time.Minute
	// Heimdall's scripts are idempotent, so transient failures may retry.
	// PR-controlled steps fail once, with a clear diagnosis, instead.
	dbAdminRetries int32 = 3
)

var dbScripts = map[string]string{
	"prepare.sql": `-- Heimdall: create a fresh, empty baseline database.
-- Runs as the superuser. Idempotent.
\set ON_ERROR_STOP on

-- The application role owns its databases and nothing else.
SELECT 'CREATE ROLE ` + dbRole + ` LOGIN'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + dbRole + `')
\gexec
ALTER ROLE ` + dbRole + ` WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS
  PASSWORD :'app_password';

-- Rebuild from scratch: no leftovers from an older generation.
SELECT 'ALTER DATABASE ` + dbBaseline + ` IS_TEMPLATE false'
 WHERE EXISTS (SELECT FROM pg_database WHERE datname = '` + dbBaseline + `')
\gexec
DROP DATABASE IF EXISTS ` + dbBaseline + ` WITH (FORCE);
CREATE DATABASE ` + dbBaseline + ` OWNER ` + dbRole + `;
`,
	"freeze.sql": `-- Heimdall: freeze the migrated, seeded baseline as a template.
-- Runs as the superuser. Idempotent.
\set ON_ERROR_STOP on
ALTER DATABASE ` + dbBaseline + ` WITH ALLOW_CONNECTIONS false;
SELECT count(pg_terminate_backend(pid)) AS disconnected
  FROM pg_stat_activity WHERE datname = '` + dbBaseline + `' AND pid <> pg_backend_pid();
ALTER DATABASE ` + dbBaseline + ` IS_TEMPLATE true;
`,
	"clone.sql": `-- Heimdall: replace the live database with a copy of the frozen baseline.
-- Used by the clone step and by reset. Runs as the superuser. Idempotent.
\set ON_ERROR_STOP on
DROP DATABASE IF EXISTS ` + dbApp + ` WITH (FORCE);
CREATE DATABASE ` + dbApp + ` TEMPLATE ` + dbBaseline + ` OWNER ` + dbRole + `;
`,
}

// waitForPostgres guards each Job against a database that is not accepting
// connections yet; activeDeadlineSeconds bounds the wait.
const waitForPostgres = `until pg_isready --quiet; do sleep 1; done; `

func (b *builder) baselineDB() Stage {
	p := b.cfg.Dependencies.Postgres
	if p == nil {
		return Stage{Name: StageBaselineDB}
	}
	stage := Stage{Name: StageBaselineDB}
	scripts := &corev1.ConfigMap{
		TypeMeta:   typeConfigMap,
		ObjectMeta: b.meta(dbScriptsConfigMap, StageBaselineDB, componentDBAdmin),
		Data:       dbScripts,
	}
	stage.Steps = append(stage.Steps, Step{Name: StepPrepare, Objects: []Object{
		scripts,
		b.dbAdminJob("prepare", p,
			[]corev1.EnvVar{credential("APP_DB_PASSWORD", keyPostgresAppPassword)},
			`psql -v ON_ERROR_STOP=1 -v app_password="$APP_DB_PASSWORD" -f `+scriptsDir+`/prepare.sql`),
	}})
	if m := b.cfg.Migrations; m != nil {
		stage.Steps = append(stage.Steps, Step{Name: StepMigrate, Objects: []Object{b.migrationJob(m)}})
	}
	if p.Seed != "" {
		stage.Steps = append(stage.Steps, Step{Name: StepSeed, Objects: b.seed(p)})
	}
	stage.Steps = append(stage.Steps, Step{Name: StepClone, Objects: []Object{
		b.dbAdminJob("clone", p, nil,
			`psql -v ON_ERROR_STOP=1 -f `+scriptsDir+`/freeze.sql -f `+scriptsDir+`/clone.sql`),
	}})
	return stage
}

// dbAdminJob runs one of Heimdall's scripts as the superuser, using psql from
// the same image (and so the same major version) as the server.
func (b *builder) dbAdminJob(step string, p *config.Postgres, env []corev1.EnvVar, script string) *batchv1.Job {
	img := b.dependencyImage(config.DepPostgres, p.Version)
	c := container("psql", img.ref(b.platform.ImageMirror), true)
	c.Command = []string{"sh", "-c", waitForPostgres + "exec " + script}
	c.Env = append([]corev1.EnvVar{
		{Name: "PGHOST", Value: config.DepPostgres},
		{Name: "PGUSER", Value: "postgres"},
		{Name: "PGDATABASE", Value: "postgres"},
		{Name: "PGCONNECT_TIMEOUT", Value: "5"},
		credential("PGPASSWORD", keyPostgresSuperuserPassword),
	}, env...)
	c.Resources = b.fixed("100m", "128Mi")
	c.VolumeMounts = []corev1.VolumeMount{readOnlyMount("scripts", scriptsDir), mount("tmp", "/tmp")}
	volumes := []corev1.Volume{configMapVolume("scripts", dbScriptsConfigMap), scratch("tmp", tmpSize)}

	name := generationName(b.ctx.Generation, "heimdall-db", step)
	return b.job(name, "heimdall-db-"+step, componentDBAdmin, StageBaselineDB, dbAdminTimeout, dbAdminRetries,
		b.podSpec(img.uid, img.gid, corev1.RestartPolicyNever, volumes, c))
}

// migrationJob runs the PR's migration command from the migration service's
// image, with that service's environment, against the baseline database.
func (b *builder) migrationJob(m *config.Migrations) *batchv1.Job {
	svc := b.cfg.Services[m.Service]
	c := container("migrate", b.images[m.Service], !b.platform.WritableRootFilesystem)
	c.Command = []string{"sh", "-c", m.Command}
	c.Env = b.workloadEnv("migrations", 0, svc.Env, svc.Secrets, baselineDatabase)
	c.Resources = b.workloadResources("services."+m.Service, svc.Resources)
	c.VolumeMounts = []corev1.VolumeMount{mount("tmp", "/tmp")}
	return b.job(generationName(b.ctx.Generation, "heimdall-migrate"), "heimdall-migrate", componentMigration,
		StageBaselineDB, m.Timeout.Std(), 0,
		b.podSpec(appUID, appGID, corev1.RestartPolicyNever, []corev1.Volume{scratch("tmp", tmpSize)}, c))
}

// seed loads the PR's seed file into the baseline, in one transaction (all or
// nothing), as the app role. The file travels in an immutable, per-generation
// ConfigMap, so a running Job can never see a newer generation's seed.
func (b *builder) seed(p *config.Postgres) []Object {
	cmName := generationName(b.ctx.Generation, "heimdall-seed")
	cm := &corev1.ConfigMap{
		TypeMeta:   typeConfigMap,
		ObjectMeta: b.meta(cmName, StageBaselineDB, componentSeed),
		Immutable:  ptr(true),
	}
	if utf8.Valid(b.ctx.Seed) {
		cm.Data = map[string]string{seedKey: string(b.ctx.Seed)}
	} else {
		cm.BinaryData = map[string][]byte{seedKey: b.ctx.Seed}
	}

	img := b.dependencyImage(config.DepPostgres, p.Version)
	c := container("psql", img.ref(b.platform.ImageMirror), true)
	c.Command = []string{"sh", "-c", `test "$HEIMDALL_NAMESPACE" = '` + b.ns + `' || exit 64; ` +
		`case "$HEIMDALL_NAMESPACE" in heimdall-*) ;; *) exit 64;; esac; ` +
		`case "$DATABASE_URL" in postgres://app:*@postgres:5432/app_baseline\?sslmode=disable) ;; *) exit 64;; esac; ` +
		`until pg_isready --quiet --dbname="$DATABASE_URL"; do sleep 1; done; ` +
		`exec psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f ` + seedDir + "/" + seedKey}
	c.Env = []corev1.EnvVar{credential(config.EnvDatabaseURL, keyDatabaseURLBaseline), {
		Name: "HEIMDALL_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
	}}
	c.Resources = b.fixed("250m", "256Mi")
	c.VolumeMounts = []corev1.VolumeMount{readOnlyMount("seed", seedDir), mount("tmp", "/tmp")}
	volumes := []corev1.Volume{configMapVolume("seed", cmName), scratch("tmp", tmpSize)}

	job := b.job(generationName(b.ctx.Generation, "heimdall-seed"), "heimdall-seed", componentSeed, StageBaselineDB,
		seedTimeout, 0, b.podSpec(img.uid, img.gid, corev1.RestartPolicyNever, volumes, c))
	return []Object{cm, job}
}

func configMapVolume(name, configMap string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
		DefaultMode:          ptr(int32(0o444)),
	}}}
}

// job wraps a pod spec in a Job that runs once, within a deadline. Jobs carry
// the generation in their name (their pod template is immutable) and are
// pruned by the engine once a newer generation succeeds (ADR 0007), rather than
// by a TTL that could delete one the engine still needs to inspect.
func (b *builder) job(name, podName, component string, stage StageName, deadline time.Duration, retries int32, spec corev1.PodSpec) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   typeJob,
		ObjectMeta: b.meta(name, stage, component),
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr(retries),
			ActiveDeadlineSeconds: ptr(int64(deadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: b.jobPodLabels(podName, component, stage)},
				Spec:       spec,
			},
		},
	}
}
