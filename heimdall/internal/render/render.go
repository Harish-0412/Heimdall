// Package render turns a loaded heimdall.yaml (config.Config) and the facts of
// one deployment (Context) into typed Kubernetes objects, grouped into the
// ordered stages of ADR 0007:
//
//	guardrails -> dependencies -> baseline-db -> application -> smoke
//
// Each stage holds ordered steps; the objects of a step are applied together
// (server-side apply, one field manager) and must be ready before the next
// step starts. Render decides what to apply; the engine (P2) decides when.
//
// Render is a pure function: no cluster access, clock, randomness or file
// system. The same inputs always produce byte-identical output, which golden
// tests, server-side apply and generation fencing rely on.
//
// Every pod carries the platform's secure defaults (security.go), which
// heimdall.yaml cannot express or weaken (ADR 0006).
package render

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// StageName identifies a pipeline stage. Stage names are public: conditions,
// events and diagnoses refer to them.
type StageName string

const (
	StageGuardrails   StageName = "guardrails"   // namespace, quota, limits, network policy, credentials
	StageDependencies StageName = "dependencies" // postgres, redis, rabbitmq
	StageBaselineDB   StageName = "baseline-db"  // prepare -> migrate -> seed -> clone
	StageApplication  StageName = "application"  // services and workers, in dependsOn waves
	StageSmoke        StageName = "smoke"        // smoke-test Jobs
)

// StageNames returns every stage in execution order.
func StageNames() []StageName {
	return []StageName{StageGuardrails, StageDependencies, StageBaselineDB, StageApplication, StageSmoke}
}

// Step names within stages.
const (
	StepSetup   = "setup"   // guardrails
	StepStart   = "start"   // dependencies
	StepPrepare = "prepare" // baseline-db: fresh baseline database
	StepMigrate = "migrate" // baseline-db: migrations Job
	StepSeed    = "seed"    // baseline-db: seed Job
	StepClone   = "clone"   // baseline-db: freeze baseline, clone the live database
	StepRun     = "run"     // smoke
	// Application steps are "wave-1", "wave-2", ... in dependsOn order.
)

// Object is a typed Kubernetes object with its TypeMeta set, ready for
// server-side apply. It matches controller-runtime's client.Object.
type Object interface {
	metav1.Object
	runtime.Object
}

// Step is a set of objects applied together and awaited together:
// Deployments and StatefulSets until available, Jobs until complete.
type Step struct {
	Name    string
	Objects []Object
}

// Stage is one pipeline stage. Steps is empty when the configuration needs
// nothing from the stage (for example baseline-db without postgres); the
// stage is still listed so every plan has the same shape.
type Stage struct {
	Name  StageName
	Steps []Step
}

// URL is a public service's preview URL.
type URL struct {
	Service string
	Primary bool
	URL     string
}

// Plan is the rendered, ordered set of objects for one deployment.
type Plan struct {
	// Namespace holds every namespaced object of the plan.
	Namespace string
	// URLs lists public services, sorted by name.
	URLs []URL
	// Stages are in execution order; see StageNames.
	Stages []Stage
}

// Stage returns the named stage.
func (p *Plan) Stage(name StageName) (Stage, bool) {
	for _, s := range p.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// Objects returns every object in apply order.
func (p *Plan) Objects() []Object {
	var out []Object
	for _, s := range p.Stages {
		for _, st := range s.Steps {
			out = append(out, st.Objects...)
		}
	}
	return out
}

// Render builds the plan for cfg, which must come from config.Load (with the
// same policy as ctx.Policy). On failure the error is an Errors value listing
// every problem.
func Render(cfg *config.Config, ctx Context) (*Plan, error) {
	if !cfg.Loaded() {
		return nil, Errors{{Code: CodeConfigNotLoaded, Message: "config must come from config.Load"}}
	}
	b := &builder{cfg: cfg, ctx: ctx, platform: ctx.Platform.withDefaults()}
	b.ctx.Platform = b.platform
	if errs := b.ctx.validate(cfg); len(errs) > 0 {
		return nil, errs
	}
	b.ns = namespaceName(ctx.PR, ctx.Repo, ctx.URLSuffix)
	b.resolveImages()
	b.resolveURLs()
	if len(b.errs) > 0 {
		return nil, b.errs
	}

	plan := &Plan{Namespace: b.ns, URLs: b.publicURLs()}
	deps := b.dependencies()
	db := b.baselineDB()
	app := b.application()
	smoke := b.smoke()
	// Guardrails come first but are built last: the quota is sized from the
	// Jobs the other stages contain.
	guard := b.guardrails(deps, db, app, smoke)
	plan.Stages = []Stage{guard, deps, db, app, smoke}
	if err := b.errs.err(); err != nil {
		return nil, err
	}
	for _, s := range plan.Stages {
		for _, st := range s.Steps {
			sortObjects(st.Objects)
		}
	}
	return plan, nil
}

type builder struct {
	cfg      *config.Config
	ctx      Context
	platform Platform
	ns       string
	images   map[string]string // workload -> pinned image
	urls     map[string]string // public service -> URL
	hosts    map[string]string // public service -> hostname
	errs     Errors
}

var (
	digestRefRE = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	digestRE    = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
)

// resolveImages decides the exact image of every workload. Only references
// pinned by digest are accepted: a tag can be moved after review, a digest
// cannot.
func (b *builder) resolveImages() {
	b.images = map[string]string{}
	declared := map[string]string{} // workload -> image: from heimdall.yaml ("" when built)
	for name, s := range b.cfg.Services {
		declared[name] = s.Image
	}
	for name, w := range b.cfg.Workers {
		declared[name] = w.Image
	}
	for _, name := range sortedKeys(b.ctx.Images) {
		if _, ok := declared[name]; !ok {
			b.errs.add(CodeImageUnknown, "Context.Images."+name, "no service or worker is named %q", name)
		}
	}
	for _, name := range sortedKeys(declared) {
		field := "Context.Images." + name
		ref, fromCtx := b.ctx.Images[name]
		cfgImage := declared[name]
		switch {
		case !fromCtx && digestRE.MatchString(cfgImage):
			ref = cfgImage // pinned in heimdall.yaml itself
		case !fromCtx && cfgImage != "":
			b.errs.add(CodeImageMissing, field, "image %q is not pinned by digest; supply the resolved digest", cfgImage)
			continue
		case !fromCtx:
			b.errs.add(CodeImageMissing, field, "no image for %q; CI must build it and supply the digest", name)
			continue
		case cfgImage != "" && repository(ref) != repository(cfgImage):
			// Policy checked the registry of the declared image; the
			// resolved image must be the same repository.
			b.errs.add(CodeImageMismatch, field, "%q is not the repository declared in heimdall.yaml (%q)", ref, cfgImage)
			continue
		}
		switch {
		case !digestRefRE.MatchString(ref):
			b.errs.add(CodeImageUnpinned, field, "%q must be a lowercase image reference pinned by digest (name@sha256:...)", ref)
		case imageTag(ref) == "latest":
			b.errs.add(CodeImageLatest, field, "%q uses the latest tag; tag images with something meaningful", ref)
		default:
			b.images[name] = ref
		}
	}
}

// splitImage splits a reference into name and tag, dropping any digest. A
// colon only starts a tag after the last '/' ("localhost:5001/x" has no tag).
func splitImage(ref string) (name, tag string) {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func imageTag(ref string) string {
	_, tag := splitImage(ref)
	return tag
}

// repository returns the normalized repository of an image reference, without
// tag or digest: "nginx:1.27" -> "docker.io/library/nginx".
func repository(ref string) string {
	ref, _ = splitImage(ref)
	first, rest, found := strings.Cut(ref, "/")
	switch {
	case !found:
		return "docker.io/library/" + ref
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		return "docker.io/" + ref
	case first == "index.docker.io":
		return "docker.io/" + rest
	}
	return ref
}

// resolveURLs assigns each public service its hostname and URL.
func (b *builder) resolveURLs() {
	b.urls, b.hosts = map[string]string{}, map[string]string{}
	primary := b.cfg.PrimaryService()
	for _, name := range sortedKeys(b.cfg.Services) {
		if !b.cfg.Services[name].Public {
			continue
		}
		svc := name
		if name == primary {
			svc = ""
		}
		host := hostLabel(b.ctx.PR, b.ctx.Repo, svc, b.ctx.URLSuffix) + "." + b.platform.BaseDomain
		url := b.platform.URLScheme + "://" + host
		if b.platform.URLPort != 0 {
			url += fmt.Sprintf(":%d", b.platform.URLPort)
		}
		b.hosts[name], b.urls[name] = host, url
	}
}

func (b *builder) publicURLs() []URL {
	var out []URL
	primary := b.cfg.PrimaryService()
	for _, name := range sortedKeys(b.urls) {
		out = append(out, URL{Service: name, Primary: name == primary, URL: b.urls[name]})
	}
	return out
}

// kindOrder applies prerequisites first: namespace, identities and config
// before the workloads that use them, routes after their backends.
var kindOrder = map[string]int{
	"Namespace": 0, "ServiceAccount": 1, "Secret": 2, "ConfigMap": 3, "ResourceQuota": 4,
	"LimitRange": 5, "NetworkPolicy": 6, "Service": 7, "StatefulSet": 8, "Deployment": 9,
	"Job": 10, "HTTPRoute": 11,
}

func sortObjects(objs []Object) {
	slices.SortStableFunc(objs, func(a, b Object) int {
		ka, kb := a.GetObjectKind().GroupVersionKind().Kind, b.GetObjectKind().GroupVersionKind().Kind
		return cmp.Or(cmp.Compare(kindOrder[ka], kindOrder[kb]), cmp.Compare(a.GetName(), b.GetName()))
	})
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
