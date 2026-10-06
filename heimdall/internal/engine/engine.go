package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

func asObject(o render.Object) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	return &unstructured.Unstructured{Object: m}, err
}
func identity(p *render.Plan) map[string]string {
	for _, o := range p.Objects() {
		if o.GetObjectKind().GroupVersionKind().Kind == "Namespace" {
			l := o.GetLabels()
			return map[string]string{render.LabelTenant: l[render.LabelTenant], render.LabelRepo: l[render.LabelRepo], render.LabelPR: l[render.LabelPR], render.LabelEnv: l[render.LabelEnv], managedBy: render.ManagedBy}
		}
	}
	return nil
}
func owned(o *unstructured.Unstructured, want map[string]string) bool {
	for k, v := range want {
		if v == "" || o.GetLabels()[k] != v {
			return false
		}
	}
	return len(want) > 0
}
func ownership(o *unstructured.Unstructured, want map[string]string) error {
	if !owned(o, want) {
		return failure("engine.ownership", "refusing to modify an object outside this environment: "+o.GetKind()+"/"+o.GetName(), nil)
	}
	return nil
}

// Digest identifies execution inputs. Expiry is metadata and credentials are
// generated runtime state, so neither changes a deployment's identity.
func (s Spec) Digest() (string, error) {
	c := s.Context
	c.Credentials = nil
	c.ExpiresAt = time.Time{}
	return hash(struct {
		Config   any
		Context  render.Context
		Approval *SeedApproval
	}{s.Config, c, s.SeedApproval})
}
func (e *Engine) plan(s Spec) (*render.Plan, error) {
	if err := validateData(s); err != nil {
		return nil, err
	}
	if s.Context.Credentials != nil {
		return nil, failure("engine.credentials", "credentials must be managed by the engine", nil)
	}
	for _, ref := range s.Context.Images {
		if strings.Contains(ref, "placeholder.invalid/") {
			return nil, failure("engine.image", "execution requires real digest-pinned images", nil)
		}
	}
	p, err := render.Render(s.Config, s.Context)
	if err != nil {
		message := "invalid deployment specification"
		var problems render.Errors
		if errors.As(err, &problems) && len(problems) > 0 {
			message += ": " + problems[0].Code + " at " + problems[0].Field
		}
		return nil, failure("engine.spec_invalid", message, err)
	}
	if !strings.HasPrefix(p.Namespace, "heimdall-") {
		return nil, failure("engine.namespace", "execution is restricted to Heimdall namespaces", nil)
	}
	return p, nil
}
func (e *Engine) ensureNamespace(ctx context.Context, p *render.Plan, create bool) (*unstructured.Unstructured, error) {
	existing, err := e.cluster.Get(ctx, namespaces, "", p.Namespace)
	if apierrors.IsNotFound(err) && create {
		for _, obj := range p.Objects() {
			if obj.GetObjectKind().GroupVersionKind().Kind == "Namespace" {
				u, err := asObject(obj)
				if err != nil {
					return nil, err
				}
				existing, err = e.cluster.Create(ctx, namespaces, u)
				if apierrors.IsAlreadyExists(err) {
					return e.ensureNamespace(ctx, p, false)
				}
				return existing, err
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = ownership(existing, identity(p)); err != nil {
		return nil, err
	}
	if existing.GetLabels()[render.LabelPreview] != "true" {
		return nil, failure("engine.namespace", "namespace is not marked as a preview", nil)
	}
	if existing.GetDeletionTimestamp() != nil {
		return existing, failure("engine.terminating", "namespace is still terminating; inspect finalizers and retry after deletion", nil)
	}
	return existing, nil
}
func (e *Engine) credentials(ctx context.Context, s Spec, p *render.Plan) (*render.Plan, error) {
	creds := render.GenerateCredentials()
	old, err := e.cluster.Get(ctx, secrets, p.Namespace, render.CredentialsSecret)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	// Missing credentials must not silently rotate an existing dependency.
	for _, dep := range []struct {
		name     string
		resource Resource
		keys     []string
	}{
		{"postgres", statefulsets, []string{"postgres-superuser-password", "postgres-app-password"}},
		{"redis", deployments, []string{"redis-password"}},
		{"rabbitmq", deployments, []string{"rabbitmq-password"}},
	} {
		missing := old == nil
		for _, key := range dep.keys {
			if old != nil && str(old, "data", key) == "" {
				missing = true
			}
		}
		if missing {
			existing, getErr := e.cluster.Get(ctx, dep.resource, p.Namespace, dep.name)
			if getErr != nil && !apierrors.IsNotFound(getErr) {
				return nil, getErr
			}
			if existing != nil {
				return nil, failure("engine.credentials_missing", "existing dependency credentials are missing; restore the Secret before retrying", nil)
			}
		}
	}
	if err == nil {
		if err = ownership(old, identity(p)); err != nil {
			return nil, err
		}
		fields := map[string]*string{"postgres-superuser-password": &creds.PostgresSuperuser, "postgres-app-password": &creds.PostgresApp, "redis-password": &creds.Redis, "rabbitmq-password": &creds.RabbitMQ}
		for key, dst := range fields {
			value := str(old, "data", key)
			if value == "" {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(b) < 16 {
				return nil, failure("engine.credentials_invalid", "stored environment credentials are invalid", err)
			}
			*dst = string(b)
		}
	}
	s.Context.Credentials = creds
	return render.Render(s.Config, s.Context)
}
func (e *Engine) step(ctx context.Context, s *session, result *Result, name string, fn func(context.Context) error) error {
	started := time.Now()
	ev := Event{Operation: s.record.Operation, Stage: name, State: "running", Generation: result.Generation, At: started.UTC()}
	if err := s.event(ctx, ev); err != nil {
		ev.State, ev.Code, ev.Duration = "failed", errorCode(err), time.Since(started)
		result.Phase = "failed"
		result.Events = append(result.Events, ev)
		if e.observe != nil {
			e.observe(ev)
		}
		return err
	}
	work, cancel := context.WithTimeout(ctx, e.timeout)
	err := fn(work)
	// A failed journal renewal cancels every operation with its real cause.
	// API clients commonly return context.Canceled; keep the ownership failure
	// rather than turning it into an unrelated Kubernetes operation failure.
	var lost *Error
	if work.Err() != nil && errors.As(context.Cause(work), &lost) && lost.Code == "engine.lock_lost" {
		err = lost
	}
	cancel()
	ev.At = time.Now().UTC()
	ev.Duration = time.Since(started)
	ev.State = "succeeded"
	if err != nil {
		ev.State = "failed"
		result.Phase = "failed"
		var typed *Error
		switch {
		case errors.As(err, &typed):
			ev.Code = typed.Code
		case errors.Is(err, context.DeadlineExceeded):
			ev.Code = "engine.timeout"
		default:
			ev.Code = "engine.cluster"
		}
	}
	result.Events = append(result.Events, ev)
	if journalErr := s.event(ctx, ev); journalErr != nil {
		if err == nil {
			err = journalErr
			ev.State, ev.Code = "failed", errorCode(journalErr)
			result.Phase = "failed"
			result.Events[len(result.Events)-1] = ev
		}
		if ev.State == "failed" && e.observe != nil {
			// The expired/lost journal cannot be written, but observers still
			// need the terminal event explaining why this step stopped.
			e.observe(ev)
		}
	}
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			return err
		}
		return failure(ev.Code, "operation failed in "+name+"; inspect status and namespace events", err)
	}
	return nil
}
func (e *Engine) applyObjects(ctx context.Context, p *render.Plan, objects []render.Object) error {
	var applied []struct {
		r Resource
		o *unstructured.Unstructured
	}
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		u, err := asObject(obj)
		if err != nil {
			return err
		}
		r, err := resourceFor(u)
		if err != nil {
			return err
		}
		existing, err := e.cluster.Get(ctx, r, u.GetNamespace(), u.GetName())
		if err == nil {
			if err = ownership(existing, identity(p)); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		live, err := e.cluster.Apply(ctx, r, u)
		if err != nil {
			return failure("engine.apply", "cannot apply "+r.Kind+"/"+u.GetName()+" ("+string(apierrors.ReasonForError(err))+")", err)
		}
		applied = append(applied, struct {
			r Resource
			o *unstructured.Unstructured
		}{r, live})
	}
	for _, item := range applied {
		switch item.r.Kind {
		case "Deployment", "StatefulSet", "Job":
			if err := e.await(ctx, item.r, item.o); err != nil {
				return err
			}
		}
	}
	return nil
}

// await waits for a workload to become ready, and stops early when one of
// its pods fails in a way that cannot recover without new input (a crash
// loop, repeated out-of-memory kills, an image that cannot be pulled):
// waiting out the step timeout would only delay the diagnosis.
func (e *Engine) await(ctx context.Context, r Resource, o *unstructured.Unstructured) error {
	selector := podSelector(o)
	if selector == "" || e.watchdog <= 0 {
		return e.cluster.Wait(ctx, r, o, false)
	}
	wait, cancel := context.WithCancelCause(ctx)
	stopped := make(chan struct{})
	defer func() {
		cancel(nil)
		<-stopped // no watchdog may access the cluster after await returns
	}()
	go func() {
		defer close(stopped)
		t := time.NewTicker(e.watchdog)
		defer t.Stop()
		for {
			select {
			case <-wait.Done():
				return
			case <-t.C:
			}
			list, err := e.cluster.List(wait, pods, o.GetNamespace(), selector)
			if err != nil {
				continue // the wait itself reports API trouble
			}
			typed := make([]corev1.Pod, 0, len(list))
			for i := range list {
				var p corev1.Pod
				if runtime.DefaultUnstructuredConverter.FromUnstructured(list[i].Object, &p) == nil && matchesTemplate(o, &p) {
					typed = append(typed, p)
				}
			}
			if b := diagnose.Blocked(typed, time.Now()); b != nil {
				cancel(failure("engine.workload_failed", r.Kind+" "+o.GetName()+" cannot become ready: "+b.Error(), nil))
				return
			}
		}
	}()
	err := e.cluster.Wait(wait, r, o, false)
	var blocked *Error
	if err != nil && errors.As(context.Cause(wait), &blocked) {
		return blocked
	}
	return err
}

// matchesTemplate keeps a rollout's watchdog from treating an old revision's
// failing pods as a failure of the replacement. Long-running pods deliberately
// do not carry generation labels (a metadata-only change must not restart
// dependencies). Compare the requested template instead, allowing API defaults
// and pod-only fields such as nodeName that are absent from the template.
func matchesTemplate(o *unstructured.Unstructured, p *corev1.Pod) bool {
	metadata, _, _ := unstructured.NestedStringMap(o.Object, "spec", "template", "metadata", "labels")
	for key, value := range metadata {
		if p.Labels[key] != value {
			return false
		}
	}
	template, found, err := unstructured.NestedMap(o.Object, "spec", "template", "spec")
	if !found || err != nil {
		return true // no template: Wait remains responsible for readiness
	}
	var spec corev1.PodSpec
	if runtime.DefaultUnstructuredConverter.FromUnstructured(template, &spec) != nil {
		return false
	}
	return diagnose.PodSpecMatchesTemplate(spec, p.Spec)
}

// podSelector selects a workload's pods.
func podSelector(o *unstructured.Unstructured) string {
	if o.GetKind() == "Job" {
		return "batch.kubernetes.io/job-name=" + o.GetName()
	}
	m, found, err := unstructured.NestedStringMap(o.Object, "spec", "selector", "matchLabels")
	if !found || err != nil || len(m) == 0 {
		return ""
	}
	return labels.SelectorFromSet(m).String()
}

// Apply advances only the explicit generation, serialises mutations across CLI
// processes, preserves failed Jobs and prunes only after every stage succeeds.
func (e *Engine) Apply(ctx context.Context, spec Spec) (*Result, error) {
	p, err := e.plan(spec)
	if err != nil {
		return nil, err
	}
	if _, err = e.ensureNamespace(ctx, p, true); err != nil {
		return nil, err
	}
	work, session, err := e.acquire(ctx, p.Namespace, identity(p))
	if err != nil {
		return nil, err
	}
	defer session.close()
	digest, err := spec.Digest()
	if err != nil {
		return nil, err
	}
	if err = session.begin(work, spec.Context.Generation, digest, "apply"); err != nil {
		return nil, err
	}
	p, err = e.credentials(work, spec, p)
	if err != nil {
		return nil, err
	}
	result := &Result{Namespace: p.Namespace, Generation: spec.Context.Generation, Phase: "applying", URLs: p.URLs}
	for _, stage := range p.Stages {
		if stage.Name == render.StageDependencies {
			if err = e.step(work, session, result, "cancel-superseded", func(c context.Context) error { return e.cancelSuperseded(c, p, spec.Context.Generation) }); err != nil {
				return result, err
			}
			if err = e.step(work, session, result, "storage", func(c context.Context) error { return e.replaceStorage(c, p) }); err != nil {
				return result, err
			}
		}
		needsBaseline := false
		if stage.Name == render.StageBaselineDB {
			for _, step := range stage.Steps {
				for _, obj := range step.Objects {
					if obj.GetObjectKind().GroupVersionKind().Kind != "Job" {
						continue
					}
					job, getErr := e.cluster.Get(work, jobs, p.Namespace, obj.GetName())
					if apierrors.IsNotFound(getErr) {
						needsBaseline = true
						continue
					}
					if getErr != nil {
						return result, getErr
					}
					ready, _ := Ready(job)
					needsBaseline = needsBaseline || !ready
				}
			}
		}
		if needsBaseline {
			if err = e.step(work, session, result, "quiesce", func(c context.Context) error { return e.quiesce(c, p) }); err != nil {
				return result, err
			}
		}
		for _, step := range stage.Steps {
			if err = e.step(work, session, result, string(stage.Name)+"/"+step.Name, func(c context.Context) error { return e.applyObjects(c, p, step.Objects) }); err != nil {
				return result, err
			}
		}
	}
	if err = e.step(work, session, result, "prune", func(c context.Context) error { return e.prune(c, p, spec.Context.Generation) }); err != nil {
		return result, err
	}
	result.Phase = "ready"
	err = session.event(work, Event{Operation: "apply", Stage: "complete", State: "ready", Generation: result.Generation, At: time.Now().UTC()})
	return result, err
}

func (e *Engine) deleteAndWait(ctx context.Context, r Resource, o *unstructured.Unstructured) error {
	if err := e.cluster.Delete(ctx, r, o); err != nil {
		return err
	}
	return e.cluster.Wait(ctx, r, o, true)
}
func (e *Engine) quiesce(ctx context.Context, p *render.Plan) error {
	items, err := e.cluster.List(ctx, deployments, p.Namespace, labels.SelectorFromSet(identity(p)).String())
	if err != nil {
		return err
	}
	for i := range items {
		o := &items[i]
		component := o.GetLabels()["app.kubernetes.io/component"]
		if component != "service" && component != "worker" {
			continue
		}
		if number(o, "spec", "replicas") != 0 {
			if err = unstructured.SetNestedField(o.Object, int64(0), "spec", "replicas"); err != nil {
				return err
			}
			o, err = e.cluster.Update(ctx, deployments, o)
			if err != nil {
				return err
			}
		}
		if err = e.cluster.Wait(ctx, deployments, o, false); err != nil {
			return err
		}
	}
	// Wait for terminating application pods too: zero Deployment replicas alone
	// does not prove that graceful shutdown and in-flight requests have drained.
	pp, err := e.cluster.List(ctx, pods, p.Namespace, labels.SelectorFromSet(identity(p)).String())
	if err != nil {
		return err
	}
	for i := range pp {
		component := pp[i].GetLabels()["app.kubernetes.io/component"]
		if component == "service" || component == "worker" {
			if err = e.cluster.Wait(ctx, pods, &pp[i], true); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) prune(ctx context.Context, p *render.Plan, generation int64) error {
	wanted := map[string]bool{}
	for _, o := range p.Objects() {
		wanted[o.GetObjectKind().GroupVersionKind().Kind+"/"+o.GetName()] = true
	}
	selector := labels.SelectorFromSet(identity(p)).String()
	for _, r := range inventory {
		items, err := e.cluster.List(ctx, r, p.Namespace, selector)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		for i := range items {
			o := &items[i]
			if wanted[r.Kind+"/"+o.GetName()] || o.GetName() == journalName || o.GetName() == render.AppSecretsSecret {
				continue
			}
			gen, err := strconv.ParseInt(o.GetLabels()[render.LabelGeneration], 10, 64)
			if err != nil || gen < 1 || gen >= generation {
				continue
			}
			if err = e.deleteAndWait(ctx, r, o); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) replaceStorage(ctx context.Context, p *render.Plan) error {
	for _, obj := range p.Objects() {
		if obj.GetObjectKind().GroupVersionKind().Kind != "StatefulSet" || obj.GetName() != "postgres" {
			continue
		}
		desired, err := asObject(obj)
		if err != nil {
			return err
		}
		templates, _, _ := unstructured.NestedSlice(desired.Object, "spec", "volumeClaimTemplates")
		if len(templates) != 1 {
			return failure("engine.storage", "unsupported Postgres claim layout", nil)
		}
		template := &unstructured.Unstructured{Object: templates[0].(map[string]any)}
		size := str(template, "spec", "resources", "requests", "storage")
		old, err := e.cluster.Get(ctx, statefulsets, p.Namespace, "postgres")
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		changed := false
		if err == nil {
			if err = ownership(old, identity(p)); err != nil {
				return err
			}
			oldTemplates, _, _ := unstructured.NestedSlice(old.Object, "spec", "volumeClaimTemplates")
			if len(oldTemplates) != 1 {
				return failure("engine.storage", "unexpected existing claim layout", nil)
			}
			previous := &unstructured.Unstructured{Object: oldTemplates[0].(map[string]any)}
			changed = str(previous, "spec", "resources", "requests", "storage") != size
			// Storage class changes also need explicit replacement, never an arbitrary
			// retry of any Invalid error (which could conceal an unrelated defect).
			changed = changed || str(previous, "spec", "storageClassName") != str(template, "spec", "storageClassName")
		}
		pvc, pvcErr := e.cluster.Get(ctx, claims, p.Namespace, "data-postgres-0")
		if pvcErr != nil && !apierrors.IsNotFound(pvcErr) {
			return pvcErr
		}
		if pvcErr == nil {
			if err = ownership(pvc, identity(p)); err != nil {
				return err
			}
			changed = changed || str(pvc, "spec", "resources", "requests", "storage") != size
		}
		if !changed {
			continue
		}
		if err = e.quiesce(ctx, p); err != nil {
			return err
		}
		// Delete dependent pods before the PVC, and verify the old PVC is gone
		// before creating the replacement, including after an interrupted attempt.
		if old != nil {
			if err = e.deleteAndWait(ctx, statefulsets, old); err != nil {
				return err
			}
		}
		pvc, pvcErr = e.cluster.Get(ctx, claims, p.Namespace, "data-postgres-0")
		if pvcErr == nil {
			if err = ownership(pvc, identity(p)); err != nil {
				return err
			}
			if err = e.deleteAndWait(ctx, claims, pvc); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(pvcErr) {
			return pvcErr
		}
	}
	return nil
}

// ExportedIdentity supplies the trusted scope for operational queries.
func (e *Engine) Scope(s Spec) (string, map[string]string, error) {
	p, err := e.plan(s)
	if err != nil {
		return "", nil, err
	}
	return p.Namespace, maps.Clone(identity(p)), nil
}
func errorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "engine.cluster"
}

func (e *Engine) cancelSuperseded(ctx context.Context, p *render.Plan, generation int64) error {
	items, err := e.cluster.List(ctx, jobs, p.Namespace, labels.SelectorFromSet(identity(p)).String())
	if err != nil {
		return err
	}
	for i := range items {
		o := &items[i]
		gen, parseErr := strconv.ParseInt(o.GetLabels()[render.LabelGeneration], 10, 64)
		if parseErr != nil || gen < 1 || gen >= generation {
			continue
		}
		terminal := false
		for _, raw := range conditions(o) {
			c, ok := raw.(map[string]any)
			if ok && c["status"] == "True" && (c["type"] == "Complete" || c["type"] == "Failed") {
				terminal = true
			}
		}
		if !terminal {
			if err = e.deleteAndWait(ctx, jobs, o); err != nil {
				return err
			}
		}
	}
	return nil
}
