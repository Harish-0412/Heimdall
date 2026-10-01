package render

import (
	"maps"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels on every object Heimdall renders. They are searchable metadata only:
// the controller never reads them back to decide what should exist (ADR 0005).
const (
	LabelTenant     = "heimdall.dev/tenant"
	LabelRepo       = "heimdall.dev/repo" // owner.name; exact value in AnnotationRepo
	LabelPR         = "heimdall.dev/pr"
	LabelEnv        = "heimdall.dev/env"
	LabelGeneration = "heimdall.dev/generation"
	LabelOwner      = "heimdall.dev/owner"
	LabelExpires    = "heimdall.dev/expires" // Unix seconds
	LabelStage      = "heimdall.dev/stage"
	// LabelPreview marks preview namespaces. The agent's admission policy and
	// the shared Gateway's allowedRoutes select on it.
	LabelPreview = "heimdall.dev/preview"
)

// Annotations carry exact values that do not fit label syntax.
const (
	AnnotationRepo       = "heimdall.dev/repo"
	AnnotationSHA        = "heimdall.dev/sha"
	AnnotationExpiresAt  = "heimdall.dev/expires-at" // RFC 3339
	AnnotationURL        = "heimdall.dev/url"
	AnnotationVisibility = "heimdall.dev/visibility" // consumed by the preview gateway (P7)
)

// Recommended Kubernetes labels.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelComponent = "app.kubernetes.io/component"
	labelPartOf    = "app.kubernetes.io/part-of"
	labelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "heimdall"
)

// Values for app.kubernetes.io/component.
const (
	componentService   = "service"
	componentWorker    = "worker"
	componentDatabase  = "database"
	componentCache     = "cache"
	componentBroker    = "broker"
	componentDBAdmin   = "database-admin"
	componentMigration = "migration"
	componentSeed      = "seed"
	componentSmokeTest = "smoke-test"
	componentGuardrail = "guardrail"
	componentRoute     = "route"
)

// envLabels are stable for the environment's whole life, so they are safe on
// pod templates: changing a template label restarts pods.
func (b *builder) envLabels() map[string]string {
	return map[string]string{
		LabelTenant:    b.ctx.Tenant,
		LabelRepo:      labelValue(strings.Replace(b.ctx.Repo, "/", ".", 1)),
		LabelPR:        strconv.Itoa(b.ctx.PR),
		LabelEnv:       b.ctx.EnvironmentID,
		LabelOwner:     labelValue(b.ctx.Owner),
		labelInstance:  b.ctx.EnvironmentID,
		labelPartOf:    repoName(b.ctx.Repo),
		labelManagedBy: ManagedBy,
	}
}

// objectLabels go on object metadata. They include values that change between
// generations (generation, expiry), which is why they are not copied onto pod
// templates of long-running workloads: a new push must not restart a database.
func (b *builder) objectLabels(stage StageName, component string) map[string]string {
	l := b.envLabels()
	l[LabelGeneration] = strconv.FormatInt(b.ctx.Generation, 10)
	l[LabelExpires] = strconv.FormatInt(b.ctx.ExpiresAt.Unix(), 10)
	l[LabelStage] = string(stage)
	if component != "" {
		l[labelComponent] = component
	}
	return l
}

// selector identifies one workload's pods. Selectors are immutable, so they
// use only the name, which cannot change without becoming a new workload.
func selector(name string) map[string]string {
	return map[string]string{labelName: name}
}

// podLabels are template labels for a long-running workload.
func (b *builder) podLabels(name, component string) map[string]string {
	l := b.envLabels()
	maps.Copy(l, selector(name))
	l[labelComponent] = component
	return l
}

// jobPodLabels add the generation and stage: Jobs are per generation anyway,
// and diagnostics map a failed pod to its stage through them (P4).
func (b *builder) jobPodLabels(name, component string, stage StageName) map[string]string {
	l := b.podLabels(name, component)
	l[LabelGeneration] = strconv.FormatInt(b.ctx.Generation, 10)
	l[LabelStage] = string(stage)
	return l
}

func (b *builder) annotations() map[string]string {
	return map[string]string{AnnotationSHA: b.ctx.SHA}
}

// meta is the metadata of a namespaced object.
func (b *builder) meta(name string, stage StageName, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   b.ns,
		Labels:      b.objectLabels(stage, component),
		Annotations: b.annotations(),
	}
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
