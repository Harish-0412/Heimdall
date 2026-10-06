package engine

import (
	"context"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// Reset is a single serialised operation with a durable, monotonic nonce.
// Completed nonces are no-ops; incomplete ones are safe to retry.
func (e *Engine) Reset(ctx context.Context, spec Spec, nonce int64) (*Result, error) {
	if nonce < 1 {
		return nil, failure("engine.reset_nonce", "reset requires a positive monotonic nonce", nil)
	}
	p, err := e.plan(spec)
	if err != nil {
		return nil, err
	}
	if _, err = e.ensureNamespace(ctx, p, false); err != nil {
		return nil, err
	}
	work, s, err := e.acquire(ctx, p.Namespace, identity(p))
	if err != nil {
		return nil, err
	}
	defer s.close()
	digest, err := spec.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	stale := nonce < s.record.ResetNonce
	incomplete := nonce > s.record.ResetNonce && s.record.ResetNonce > 0 && !s.record.ResetComplete
	s.mu.Unlock()
	if stale {
		return nil, failure("engine.stale_reset", "reset nonce is older than the accepted reset", nil)
	}
	if incomplete {
		return nil, failure("engine.reset_incomplete", "retry the incomplete reset nonce before starting another reset", nil)
	}
	if err = s.begin(work, spec.Context.Generation, digest, "reset"); err != nil {
		return nil, err
	}
	result := &Result{Namespace: p.Namespace, Generation: spec.Context.Generation, Phase: "resetting", URLs: p.URLs}
	s.mu.Lock()
	if nonce == s.record.ResetNonce && s.record.ResetComplete {
		s.mu.Unlock()
		result.Phase = "ready"
		return result, s.event(work, Event{Operation: "reset", Stage: "complete", State: "ready", Generation: result.Generation, At: time.Now().UTC()})
	}
	if nonce > s.record.ResetNonce {
		s.record.ResetNonce = nonce
		s.record.ResetComplete = false
	}
	err = s.save(work)
	s.mu.Unlock()
	if err != nil {
		return result, err
	}
	database, smoke, err := render.ResetJobs(spec.Config, spec.Context, nonce)
	if err != nil {
		return result, err
	}
	actions := []struct {
		name string
		run  func(context.Context) error
	}{
		{"quiesce", func(c context.Context) error { return e.quiesce(c, p) }},
		{"database", func(c context.Context) error { return e.applyObjects(c, p, database) }},
		{"cache-and-broker", func(c context.Context) error { return e.resetEphemeral(c, p) }},
		{"resume", func(c context.Context) error {
			stage, _ := p.Stage(render.StageApplication)
			for _, step := range stage.Steps {
				if err := e.applyObjects(c, p, step.Objects); err != nil {
					return err
				}
			}
			return nil
		}},
		{"smoke", func(c context.Context) error { return e.applyObjects(c, p, smoke) }},
	}
	for _, a := range actions {
		if err = e.step(work, s, result, a.name, a.run); err != nil {
			return result, err
		}
	}
	s.mu.Lock()
	s.record.ResetComplete = true
	err = s.save(work)
	s.mu.Unlock()
	if err != nil {
		return result, err
	}
	result.Phase = "ready"
	err = s.event(work, Event{Operation: "reset", Stage: "complete", State: "ready", Generation: result.Generation, At: time.Now().UTC()})
	return result, err
}
func (e *Engine) resetEphemeral(ctx context.Context, p *render.Plan) error {
	stage, _ := p.Stage(render.StageDependencies)
	for _, step := range stage.Steps {
		for _, obj := range step.Objects {
			if obj.GetObjectKind().GroupVersionKind().Kind != "Deployment" {
				continue
			}
			if obj.GetName() != "redis" && obj.GetName() != "rabbitmq" {
				continue
			}
			// Recreate emptyDir-backed dependencies. This flushes every Redis database
			// and RabbitMQ queue, with application topology re-declared on resume.
			live, err := e.cluster.Get(ctx, deployments, p.Namespace, obj.GetName())
			if err != nil {
				return err
			}
			if err = ownership(live, identity(p)); err != nil {
				return err
			}
			if err = e.deleteAndWait(ctx, deployments, live); err != nil {
				return err
			}
			if err = e.applyObjects(ctx, p, []render.Object{obj}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Destroy never strips finalizers or force-deletes. A failed wait leaves the
// namespace and evidence available for diagnosis or explicit break-glass.
func (e *Engine) Destroy(ctx context.Context, spec Spec) (*Result, error) {
	p, err := render.CleanupPlan(spec.Context)
	if err != nil {
		return nil, err
	}
	result := &Result{Namespace: p.Namespace, Generation: spec.Context.Generation, Phase: "destroying"}
	ns, err := e.ensureNamespace(ctx, p, false)
	if apierrors.IsNotFound(err) {
		result.Phase = "destroyed"
		return result, nil
	}
	if errorCode(err) == "engine.terminating" {
		wait, cancel := context.WithTimeout(ctx, e.timeout)
		defer cancel()
		err = e.cluster.Wait(wait, namespaces, ns, true)
		if err == nil {
			result.Phase = "destroyed"
		}
		return result, err
	}
	if err != nil {
		return result, err
	}
	work, s, err := e.acquire(ctx, p.Namespace, identity(p))
	if err != nil {
		return result, err
	}
	defer s.close()
	digest, err := spec.Digest()
	if err != nil {
		return result, err
	}
	if err = s.begin(work, spec.Context.Generation, digest, "destroy"); err != nil {
		return result, err
	}
	return e.destroyContents(work, s, result, p.Namespace, identity(p))
}

// identityKeys are the labels that scope an environment's ownership.
var identityKeys = []string{render.LabelTenant, render.LabelRepo, render.LabelPR, render.LabelEnv, managedBy}

// DestroyOrphan removes a preview namespace that no authoritative desired
// state wants any more (the agent's sweeper decides that; ADR 0005). The
// namespace's own labels only scope what may be touched, never intent: every
// object deleted must carry the same identity. Deletion follows Destroy's
// order and rules, is serialised by the same journal (an active operation
// makes this fail with engine.busy), and never removes finalizers.
func (e *Engine) DestroyOrphan(ctx context.Context, name string) (*Result, error) {
	result := &Result{Namespace: name, Phase: "destroying"}
	ns, err := e.cluster.Get(ctx, namespaces, "", name)
	if apierrors.IsNotFound(err) {
		result.Phase = "destroyed"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	id := map[string]string{}
	for _, key := range identityKeys {
		v := ns.GetLabels()[key]
		if v == "" {
			return result, failure("engine.ownership", "namespace "+name+" lacks Heimdall ownership labels", nil)
		}
		id[key] = v
	}
	if id[managedBy] != render.ManagedBy || ns.GetLabels()[render.LabelPreview] != "true" || !strings.HasPrefix(name, render.NamespacePrefix) {
		return result, failure("engine.ownership", "namespace "+name+" is not a Heimdall preview", nil)
	}
	if ns.GetDeletionTimestamp() != nil {
		wait, cancel := context.WithTimeout(ctx, e.timeout)
		defer cancel()
		if err = e.cluster.Wait(wait, namespaces, ns, true); err != nil {
			return result, err
		}
		result.Phase = "destroyed"
		return result, nil
	}
	work, s, err := e.acquire(ctx, name, id)
	if err != nil {
		return result, err
	}
	defer s.close()
	s.mu.Lock()
	s.record.Operation = "destroy"
	s.record.Phase = "destroying"
	result.Generation = s.record.Generation
	err = s.save(work)
	s.mu.Unlock()
	if err != nil {
		return result, err
	}
	return e.destroyContents(work, s, result, name, id)
}

// Prepare creates the environment's namespace if it does not exist and checks
// that it is an owned, live preview namespace. A caller whose rights inside
// preview namespaces come from a per-namespace RoleBinding (the agent) binds
// it between Prepare and Apply.
func (e *Engine) Prepare(ctx context.Context, spec Spec) (string, error) {
	p, err := e.plan(spec)
	if err != nil {
		return "", err
	}
	_, err = e.ensureNamespace(ctx, p, true)
	return p.Namespace, err
}

// destroyContents deletes workloads, then pods and claims, then the
// namespace, verifying each is gone.
func (e *Engine) destroyContents(work context.Context, s *session, result *Result, namespace string, id map[string]string) (*Result, error) {
	var err error
	selector := labels.SelectorFromSet(id).String()
	for _, r := range []Resource{jobs, deployments, statefulsets, pods, claims} {
		if err = e.step(work, s, result, "destroy/"+r.GVR.Resource, func(c context.Context) error {
			objects, err := e.cluster.List(c, r, namespace, selector)
			if err != nil {
				return err
			}
			for i := range objects {
				if err = e.deleteAndWait(c, r, &objects[i]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	// Persist the last event before deleting its namespace.
	if err = s.event(work, Event{Operation: "destroy", Stage: "namespace", State: "destroying", Generation: result.Generation, At: time.Now().UTC()}); err != nil {
		return result, err
	}
	// The journal itself is about to disappear. Stop renewal without cancelling
	// the deletion watch; a missing journal during namespace GC is expected.
	s.renewCancel()
	<-s.stopped
	wait, cancel := context.WithTimeout(work, e.timeout)
	defer cancel()
	s.mu.Lock()
	leaseUntil := s.record.LeaseUntil
	s.mu.Unlock()
	// Once renewal stops, namespace mutations must finish within the lease.
	// Otherwise a newer operation could acquire the journal while this old
	// destroy is still capable of deleting its namespace. Only the read-only
	// deletion watch may outlive ownership after Delete has been accepted.
	remove, removeCancel := context.WithDeadline(wait, leaseUntil)
	defer removeCancel()
	leaseError := func(err error) error {
		if work.Err() == nil && !time.Now().Before(leaseUntil) {
			return s.lose(err)
		}
		return err
	}
	ns, err := e.cluster.Get(remove, namespaces, "", namespace)
	if err != nil {
		return result, leaseError(err)
	}
	if err = ownership(ns, id); err != nil {
		return result, err
	}
	if err = remove.Err(); err != nil {
		return result, leaseError(err)
	}
	if err = e.cluster.Delete(remove, namespaces, ns); err != nil {
		return result, leaseError(err)
	}
	removeCancel()
	if err = e.cluster.Wait(wait, namespaces, ns, true); err != nil {
		return result, failure("engine.destroy_stuck", "namespace deletion did not finish; inspect namespace conditions and finalizers", err)
	}
	result.Phase = "destroyed"
	return result, nil
}

type ObjectStatus struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Code  string `json:"code,omitempty"`
}
type Status struct {
	Result
	Objects             []ObjectStatus `json:"objects"`
	NamespaceConditions []any          `json:"namespaceConditions,omitempty"`
}

func (e *Engine) Status(ctx context.Context, spec Spec) (*Status, error) {
	p, err := e.plan(spec)
	if err != nil {
		return nil, err
	}
	result := &Status{Result: Result{Namespace: p.Namespace, Generation: spec.Context.Generation, Phase: "absent", URLs: p.URLs}}
	ns, err := e.ensureNamespace(ctx, p, false)
	switch {
	case apierrors.IsNotFound(err):
		return result, nil
	case errorCode(err) == "engine.terminating":
		result.Phase = "destroying"
		result.NamespaceConditions = conditions(ns)
		return result, nil
	case err != nil:
		return nil, err
	}
	recordObject, err := e.cluster.Get(ctx, configmaps, p.Namespace, journalName)
	if err != nil {
		return nil, err
	}
	if err = ownership(recordObject, identity(p)); err != nil {
		return nil, err
	}
	rec, err := decodeRecord(recordObject)
	if err != nil {
		return nil, err
	}
	result.Generation = rec.Generation
	result.Phase = rec.Phase
	result.Events = rec.Events
	for _, r := range []Resource{deployments, statefulsets, jobs} {
		items, err := e.cluster.List(ctx, r, p.Namespace, labels.SelectorFromSet(identity(p)).String())
		if err != nil {
			return nil, err
		}
		for i := range items {
			ready, err := Ready(&items[i])
			code := ""
			if err != nil {
				code = errorCode(err)
			}
			result.Objects = append(result.Objects, ObjectStatus{r.Kind, items[i].GetName(), ready, code})
			if !ready && result.Phase == "ready" && r.Kind != "Job" {
				result.Phase = "degraded"
			}
		}
	}
	return result, nil
}

func (e *Engine) LogPods(ctx context.Context, spec Spec, workload string) ([]unstructured.Unstructured, error) {
	p, err := e.plan(spec)
	if err != nil {
		return nil, err
	}
	if _, err = e.ensureNamespace(ctx, p, false); err != nil {
		return nil, err
	}
	scope := identity(p)
	if workload != "" {
		scope["app.kubernetes.io/name"] = workload
	}
	return e.cluster.List(ctx, pods, p.Namespace, labels.SelectorFromSet(scope).String())
}
