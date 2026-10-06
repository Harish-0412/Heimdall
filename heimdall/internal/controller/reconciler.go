package controller

import (
	"context"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// Finalizer holds a PreviewEnvironment until its resources are verified gone.
const Finalizer = "heimdall.dev/environment"

// Guard reports whether the agent may change the cluster. The agent's guard
// is its admission-policy self-check (charts/heimdall-agent README).
type Guard interface{ Allowed() bool }

// Allowed is a Guard that always allows (tests, or policy check disabled).
type Allowed struct{}

func (Allowed) Allowed() bool { return true }

// Access grants the agent its namespace-scoped role in a preview namespace.
// It must be idempotent.
type Access func(ctx context.Context, namespace string) error

// Reconciler drives PreviewEnvironment objects to their desired state.
type Reconciler struct {
	Client client.Client
	// Reader reads PreviewEnvironments through the API server. Status is the
	// record of completed work, so each reconcile must see the reconciler's
	// own last write; a cache can lag behind it. Defaults to the manager's
	// API reader.
	Reader   client.Reader
	Recorder events.EventRecorder
	Specs    SpecBuilder
	Runner   *Runner
	Access   Access
	Guard    Guard
	Clock    clock.PassiveClock
	// Resync re-checks Ready environments for drift.
	Resync time.Duration
	// BaseBackoff is the first retry delay after a retryable failure; it
	// doubles per attempt up to MaxBackoff. Defaults: 10s and 10m.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// GuardRetry is how often to re-check a closed Guard. Default 15s.
	GuardRetry time.Duration
	Metrics    *Metrics
	// Diagnoser explains failed operations in status.diagnoses; nil skips it.
	Diagnoser Diagnoser
}

// Reconcile is level-triggered and idempotent, and never waits for engine
// work: it records progress, starts or cancels operations, and consumes
// their results.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pe := &v1alpha1.PreviewEnvironment{}
	if err := r.Reader.Get(ctx, req.NamespacedName, pe); err != nil {
		if apierrors.IsNotFound(err) {
			r.Runner.Forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	log := logf.FromContext(ctx).WithValues("tenant", pe.Spec.Tenant, "repo", pe.Spec.Repository,
		"pr", pe.Spec.PullRequest, "env_id", pe.Spec.EnvironmentID, "generation", pe.Spec.Generation)
	ctx = logf.IntoContext(ctx, log)

	deleting := !pe.DeletionTimestamp.IsZero()
	if deleting && !controllerutil.ContainsFinalizer(pe, Finalizer) {
		return ctrl.Result{}, nil
	}
	if !deleting && !controllerutil.ContainsFinalizer(pe, Finalizer) {
		// Before any work: an object without the finalizer could be deleted
		// while its namespace keeps running.
		patch := client.MergeFromWithOptions(pe.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(pe, Finalizer)
		return ctrl.Result{}, r.Client.Patch(ctx, pe, patch)
	}

	original := pe.DeepCopy()
	st := &pe.Status
	st.ObservedGeneration = pe.Generation
	now := r.Clock.Now()

	op := r.Runner.Get(req.NamespacedName)
	if op != nil && op.Done() {
		finalize := r.consume(ctx, pe, op, now)
		if finalize {
			return ctrl.Result{}, r.removeFinalizer(ctx, pe, req.NamespacedName, op)
		}
		if err := r.patchStatus(ctx, original, pe); err != nil {
			return ctrl.Result{}, err // keep op: its result is consumed again next time
		}
		r.Runner.Remove(req.NamespacedName, op)
		original, op = pe.DeepCopy(), nil
	}
	if op == nil && st.Operation != nil && (st.Operation.Result == v1alpha1.ResultRunning || st.Operation.Result == v1alpha1.ResultQueued) {
		// In flight according to status, unknown to this process: the agent
		// restarted or lost leadership. The engine resumes safely.
		st.Operation.Result = v1alpha1.ResultInterrupted
		st.Operation.Attempts++
		st.Operation.CompletedAt = &metav1.Time{Time: now}
		st.Operation.RetryAfter = r.retryAfter(st.Operation.Attempts, now)
		st.LastError = &v1alpha1.ErrorStatus{Code: CodeInterrupted, Message: "the agent stopped during the operation; resuming",
			Generation: st.Operation.Generation, Retryable: true, At: metav1.NewTime(now)}
		st.Phase = v1alpha1.PhaseQueued
	}

	if op != nil {
		if current(pe, op.key) {
			r.progress(pe, op)
			return ctrl.Result{}, r.patchStatus(ctx, original, pe)
		}
		// Superseded (newer generation, reset, deletion): cancel and wait for
		// the operation to report that it stopped.
		op.Cancel()
		r.eventf(pe, corev1.EventTypeNormal, "Superseded", "Cancel", "cancelling %s of generation %d", op.key.Type, op.key.Generation)
		return ctrl.Result{}, r.patchStatus(ctx, original, pe)
	}

	want, ok := r.desired(pe)
	if !ok {
		if deleting {
			return ctrl.Result{}, r.removeFinalizer(ctx, pe, req.NamespacedName, nil)
		}
		requeue := r.checkHealth(ctx, pe)
		setSummaryConditions(st, pe.Generation, false)
		return ctrl.Result{RequeueAfter: requeue}, r.patchStatus(ctx, original, pe)
	}

	if wait, blocked := r.backoff(pe, want, now); blocked || wait > 0 {
		// Resuming interrupted work needs no input: that is still progress.
		setSummaryConditions(st, pe.Generation, st.Operation.Result == v1alpha1.ResultInterrupted)
		return ctrl.Result{RequeueAfter: wait}, r.patchStatus(ctx, original, pe)
	}
	if !r.Guard.Allowed() {
		log.Info("waiting for the agent's admission policy to be enforced before changing the cluster")
		return ctrl.Result{RequeueAfter: r.GuardRetry}, r.patchStatus(ctx, original, pe)
	}
	spec, diags, err := r.Specs.Build(ctx, pe)
	if err != nil {
		r.fail(pe, want, err, now)
		pe.Status.Diagnoses = statusDiagnoses(configReport(err, diags))
		setSummaryConditions(st, pe.Generation, false)
		wait, _ := r.backoff(pe, want, now)
		return ctrl.Result{RequeueAfter: wait}, r.patchStatus(ctx, original, pe)
	}
	r.start(ctx, pe, req.NamespacedName, want, spec, now)
	return ctrl.Result{}, r.patchStatus(ctx, original, pe)
}

// current reports whether an operation is still work the spec asks for. It
// depends on the spec alone, never on status: an operation's result counts
// only while this holds (generation fencing, ADR 0005), and a running
// operation for which it stops holding is cancelled.
func current(pe *v1alpha1.PreviewEnvironment, key opKey) bool {
	destroy := !pe.DeletionTimestamp.IsZero() || pe.Spec.DesiredState == v1alpha1.DesiredDestroyed
	switch key.Type {
	case v1alpha1.OperationDestroy:
		return destroy
	case v1alpha1.OperationApply:
		return !destroy && key.Generation == pe.Spec.Generation
	default:
		return !destroy && key.Generation == pe.Spec.Generation && key.Nonce == pe.Spec.ResetNonce
	}
}

// desired is the operation the spec asks for, given what status says is done.
// DeployedGeneration is zero when nothing is deployed (never, or destroyed).
func (r *Reconciler) desired(pe *v1alpha1.PreviewEnvironment) (opKey, bool) {
	gen, st := pe.Spec.Generation, pe.Status
	if !pe.DeletionTimestamp.IsZero() || pe.Spec.DesiredState == v1alpha1.DesiredDestroyed {
		done := st.Phase == v1alpha1.PhaseDestroyed && st.Operation != nil &&
			st.Operation.Type == v1alpha1.OperationDestroy && st.Operation.Result == v1alpha1.ResultSucceeded
		if done {
			return opKey{}, false
		}
		return opKey{Type: v1alpha1.OperationDestroy, Generation: gen}, true
	}
	// An apply of this generation that did not succeed (failed, interrupted,
	// waiting for the environment's lock) is still wanted, even when an
	// earlier apply of the same generation succeeded: it was a repair.
	applyUnfinished := st.Operation != nil && st.Operation.Type == v1alpha1.OperationApply &&
		st.Operation.Generation == gen && st.Operation.Result != v1alpha1.ResultSucceeded
	if st.DeployedGeneration < gen || st.Phase == v1alpha1.PhaseDegraded || applyUnfinished {
		return opKey{Type: v1alpha1.OperationApply, Generation: gen}, true
	}
	if pe.Spec.ResetNonce > st.CompletedResetNonce {
		return opKey{Type: v1alpha1.OperationReset, Generation: gen, Nonce: pe.Spec.ResetNonce}, true
	}
	return opKey{}, false
}

// backoff returns how long to wait before retrying the same operation, or
// blocked when its last failure needs new input instead of a retry.
func (r *Reconciler) backoff(pe *v1alpha1.PreviewEnvironment, want opKey, now time.Time) (time.Duration, bool) {
	o := pe.Status.Operation
	if o == nil || o.Type != want.Type || o.Generation != want.Generation || o.ResetNonce != want.Nonce {
		return 0, false
	}
	if o.Result != v1alpha1.ResultFailed && o.Result != v1alpha1.ResultInterrupted {
		return 0, false
	}
	if le := pe.Status.LastError; o.Result == v1alpha1.ResultFailed && le != nil && !le.Retryable {
		return 0, true
	}
	if o.RetryAfter == nil {
		return 0, false
	}
	return max(o.RetryAfter.Sub(now), 0), false
}

// retryAfter schedules the next attempt: exponential in the attempts so far,
// rounded up to a whole second because status times are stored in seconds
// (rounding down could retry early).
func (r *Reconciler) retryAfter(attempts int32, now time.Time) *metav1.Time {
	delay := r.MaxBackoff
	if attempts > 0 && attempts < 16 {
		delay = min(r.BaseBackoff<<(attempts-1), r.MaxBackoff)
	}
	at := now.Add(delay)
	if rounded := at.Truncate(time.Second); !rounded.Equal(at) {
		at = rounded.Add(time.Second)
	}
	return &metav1.Time{Time: at}
}

// start launches an operation and records it as queued.
func (r *Reconciler) start(ctx context.Context, pe *v1alpha1.PreviewEnvironment, name types.NamespacedName, key opKey, spec engine.Spec, now time.Time) {
	st := &pe.Status
	attempts := int32(0)
	if o := st.Operation; o != nil && o.Type == key.Type && o.Generation == key.Generation && o.ResetNonce == key.Nonce && o.Result != v1alpha1.ResultSucceeded {
		attempts = o.Attempts
	}
	st.Operation = &v1alpha1.OperationStatus{Type: key.Type, Generation: key.Generation, ResetNonce: key.Nonce,
		Result: v1alpha1.ResultQueued, Attempts: attempts, StartedAt: metav1.NewTime(now)}
	st.Steps = nil
	st.Phase = v1alpha1.PhaseQueued
	if key.Type == v1alpha1.OperationApply {
		setStageConditions(st, pe.Generation, nil, false)
	}
	setSummaryConditions(st, pe.Generation, true)
	namespace := render.NamespaceFor(pe.Spec.Repository, int(pe.Spec.PullRequest), pe.Spec.URLSuffix)
	run := r.operate(key, spec, namespace)
	r.Runner.Launch(name, key, now, func(ctx context.Context, op Operator) (*engine.Result, error) {
		result, err := run(ctx, op)
		return result, r.explain(ctx, spec, result, err)
	})
	logf.FromContext(ctx).Info("operation started", "operation", key.Type, "reset_nonce", key.Nonce)
	r.eventf(pe, corev1.EventTypeNormal, "Started", string(key.Type), "%s of generation %d started", key.Type, key.Generation)
}

// operate returns the engine work for key.
func (r *Reconciler) operate(key opKey, spec engine.Spec, namespace string) func(context.Context, Operator) (*engine.Result, error) {
	access := r.Access
	if access == nil {
		access = func(context.Context, string) error { return nil }
	}
	return func(ctx context.Context, op Operator) (*engine.Result, error) {
		switch key.Type {
		case v1alpha1.OperationApply:
			ns, err := op.Prepare(ctx, spec)
			if err != nil {
				return nil, err
			}
			if err = access(ctx, ns); err != nil {
				return nil, &Failure{Code: CodeAccess, Message: "cannot grant the agent access to " + ns, Retryable: true, cause: err}
			}
			return op.Apply(ctx, spec)
		case v1alpha1.OperationReset:
			if err := access(ctx, namespace); err != nil {
				return nil, &Failure{Code: CodeAccess, Message: "cannot grant the agent access to " + namespace, Retryable: true, cause: err}
			}
			return op.Reset(ctx, spec, key.Nonce)
		default:
			// Access may be impossible once the namespace is gone or
			// terminating; Destroy then only waits for it to disappear.
			_ = access(ctx, namespace)
			return op.Destroy(ctx, spec)
		}
	}
}

// progress copies a running operation's steps into status.
func (r *Reconciler) progress(pe *v1alpha1.PreviewEnvironment, op *operation) {
	s := op.snapshot()
	st := &pe.Status
	if st.Operation == nil {
		st.Operation = &v1alpha1.OperationStatus{Type: op.key.Type, Generation: op.key.Generation, ResetNonce: op.key.Nonce, StartedAt: metav1.NewTime(op.started)}
	}
	st.Steps = stepStatuses(s.steps)
	st.Operation.Result, st.Phase = v1alpha1.ResultQueued, v1alpha1.PhaseQueued
	if s.running {
		st.Operation.Result = v1alpha1.ResultRunning
		st.Phase = map[v1alpha1.OperationType]v1alpha1.Phase{
			v1alpha1.OperationApply: v1alpha1.PhaseProvisioning, v1alpha1.OperationReset: v1alpha1.PhaseResetting,
			v1alpha1.OperationDestroy: v1alpha1.PhaseDestroying,
		}[op.key.Type]
	}
	if op.key.Type == v1alpha1.OperationApply {
		setStageConditions(st, pe.Generation, st.Steps, false)
	}
	setSummaryConditions(st, pe.Generation, true)
}

// consume records a finished operation. Results only count for the work the
// spec still asks for: a result for an older generation or reset nonce is
// discarded (generation fencing, ADR 0005). It reports whether the object
// can now be released.
func (r *Reconciler) consume(ctx context.Context, pe *v1alpha1.PreviewEnvironment, op *operation, now time.Time) bool {
	s := op.snapshot()
	cancelled, evs, result, err, finished := s.cancelled, s.steps, s.result, s.err, s.finished
	st := &pe.Status
	stale := !current(pe, op.key)
	if st.Operation == nil || st.Operation.Type != op.key.Type || st.Operation.Generation != op.key.Generation || st.Operation.ResetNonce != op.key.Nonce {
		st.Operation = &v1alpha1.OperationStatus{Type: op.key.Type, Generation: op.key.Generation, ResetNonce: op.key.Nonce, StartedAt: metav1.NewTime(op.started)}
	}
	st.Operation.CompletedAt = &metav1.Time{Time: finished}
	st.Steps = stepStatuses(evs)
	duration := finished.Sub(op.started)
	log := logf.FromContext(ctx).WithValues("operation", op.key.Type, "operation_generation", op.key.Generation, "duration", duration.Round(time.Millisecond))

	switch {
	case cancelled || stale:
		st.Operation.Result = v1alpha1.ResultCancelled
		if err == nil {
			r.Metrics.staleResult()
			log.Info("discarding the result of superseded work")
			r.eventf(pe, corev1.EventTypeNormal, "StaleResultDiscarded", string(op.key.Type),
				"the result of %s for generation %d was discarded: newer input supersedes it", op.key.Type, op.key.Generation)
		}
		r.Metrics.operation(op.key.Type, v1alpha1.ResultCancelled, duration)
		return false
	case err == nil:
		r.Metrics.operation(op.key.Type, v1alpha1.ResultSucceeded, duration)
		r.Metrics.steps(evs)
		st.Operation.Result, st.Operation.Attempts, st.Operation.RetryAfter, st.LastError = v1alpha1.ResultSucceeded, 0, nil, nil
		st.Diagnoses = nil
		log.Info("operation succeeded")
		r.eventf(pe, corev1.EventTypeNormal, "Succeeded", string(op.key.Type), "%s of generation %d succeeded in %s",
			op.key.Type, op.key.Generation, duration.Round(time.Second))
		switch op.key.Type {
		case v1alpha1.OperationApply:
			st.DeployedGeneration, st.Phase = op.key.Generation, v1alpha1.PhaseReady
			if result != nil {
				st.Namespace, st.URLs = result.Namespace, urls(result.URLs)
			}
			setStageConditions(st, pe.Generation, st.Steps, true)
		case v1alpha1.OperationReset:
			st.CompletedResetNonce, st.Phase = op.key.Nonce, v1alpha1.PhaseReady
		case v1alpha1.OperationDestroy:
			// Nothing is deployed any more; returning to Running re-applies.
			st.Phase, st.URLs, st.DeployedGeneration, st.CompletedResetNonce = v1alpha1.PhaseDestroyed, nil, 0, pe.Spec.ResetNonce
			return !pe.DeletionTimestamp.IsZero()
		}
	case classify(err).Code == CodeBusy:
		// Another holder has the environment's journal lease: usually an
		// agent killed mid-operation, whose lease lapses within a minute, or
		// an administrator's CLI. Nothing failed; wait at a steady pace.
		r.Metrics.operation(op.key.Type, v1alpha1.ResultInterrupted, duration)
		st.Operation.Result, st.Operation.RetryAfter = v1alpha1.ResultInterrupted, r.retryAfter(1, now)
		st.LastError = &v1alpha1.ErrorStatus{Code: CodeBusy, Generation: op.key.Generation, Retryable: true, At: metav1.NewTime(now),
			Message: "another operation holds this environment's lock; waiting for it to finish or lapse"}
		st.Phase = v1alpha1.PhaseQueued
		setSummaryConditions(st, pe.Generation, true)
		log.Info("environment busy; waiting", "retry_after", st.Operation.RetryAfter.Time)
		return false
	default:
		f := classify(err)
		r.Metrics.operation(op.key.Type, v1alpha1.ResultFailed, duration)
		st.Operation.Result = v1alpha1.ResultFailed
		st.Operation.Attempts++
		if f.Retryable {
			st.Operation.RetryAfter = r.retryAfter(st.Operation.Attempts, now)
		}
		step := f.Step
		for _, s := range st.Steps {
			if s.State == "failed" {
				step = s.Name
			}
		}
		st.LastError = &v1alpha1.ErrorStatus{Code: f.Code, Message: f.Message, Step: step, Generation: op.key.Generation,
			Retryable: f.Retryable, At: metav1.NewTime(now)}
		st.Phase = v1alpha1.PhaseFailed
		if op.key.Type == v1alpha1.OperationApply {
			setStageConditions(st, pe.Generation, st.Steps, false)
		}
		log.Error(err, "operation failed", "code", f.Code, "retryable", f.Retryable, "step", step)
		r.eventf(pe, corev1.EventTypeWarning, "Failed", string(op.key.Type), "%s failed at %s: %s: %s", op.key.Type, step, f.Code, f.Message)
		if report := reportOf(err); report != nil {
			st.Diagnoses = statusDiagnoses(report)
			if root := report.RootCause(); root != nil {
				log.Info("diagnosed", "diagnosis", root.Code, "subject", root.Subject, "summary", root.Summary)
				r.eventf(pe, corev1.EventTypeWarning, "Diagnosed", string(op.key.Type), "%s: %s", root.Code, root.Summary)
			}
		}
	}
	setSummaryConditions(st, pe.Generation, false)
	return false
}

// fail records a failure that happened before an operation could start.
func (r *Reconciler) fail(pe *v1alpha1.PreviewEnvironment, key opKey, err error, now time.Time) {
	f := classify(err)
	st := &pe.Status
	attempts := int32(1)
	if o := st.Operation; o != nil && o.Type == key.Type && o.Generation == key.Generation && o.ResetNonce == key.Nonce {
		attempts = o.Attempts + 1
	}
	st.Operation = &v1alpha1.OperationStatus{Type: key.Type, Generation: key.Generation, ResetNonce: key.Nonce,
		Result: v1alpha1.ResultFailed, Attempts: attempts, StartedAt: metav1.NewTime(now), CompletedAt: &metav1.Time{Time: now}}
	if f.Retryable {
		st.Operation.RetryAfter = r.retryAfter(attempts, now)
	}
	st.LastError = &v1alpha1.ErrorStatus{Code: f.Code, Message: f.Message, Generation: key.Generation, Retryable: f.Retryable, At: metav1.NewTime(now)}
	st.Phase = v1alpha1.PhaseFailed
	setSummaryConditions(st, pe.Generation, false)
	r.eventf(pe, corev1.EventTypeWarning, "Rejected", string(key.Type), "%s: %s", f.Code, f.Message)
}

// checkHealth compares a Ready environment with the cluster. Missing or
// unready workloads mark it Degraded, which the next reconcile repairs by
// re-applying the same generation (self-healing). It returns the requeue.
func (r *Reconciler) checkHealth(ctx context.Context, pe *v1alpha1.PreviewEnvironment) time.Duration {
	st := &pe.Status
	if st.Phase != v1alpha1.PhaseReady || r.Resync <= 0 {
		return 0
	}
	spec, _, err := r.Specs.Build(ctx, pe)
	if err != nil {
		return r.Resync // the next apply reports it; nothing to repair now
	}
	status, err := r.Runner.Operator().Status(ctx, spec)
	if err != nil {
		logf.FromContext(ctx).Error(err, "health check failed")
		return r.Resync
	}
	degraded := status.Phase == "absent"
	for _, o := range status.Objects {
		if o.Kind != "Job" && !o.Ready {
			degraded = true
		}
	}
	if degraded {
		st.Phase = v1alpha1.PhaseDegraded
		r.eventf(pe, corev1.EventTypeWarning, "Degraded", "HealthCheck", "workloads are missing or not ready; re-applying generation %d", pe.Spec.Generation)
		return time.Second
	}
	return r.Resync
}

func (r *Reconciler) removeFinalizer(ctx context.Context, pe *v1alpha1.PreviewEnvironment, name types.NamespacedName, op *operation) error {
	patch := client.MergeFromWithOptions(pe.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if !controllerutil.RemoveFinalizer(pe, Finalizer) {
		return nil
	}
	if err := r.Client.Patch(ctx, pe, patch); err != nil {
		return client.IgnoreNotFound(err)
	}
	if op != nil {
		r.Runner.Remove(name, op)
	}
	logf.FromContext(ctx).Info("environment destroyed; released")
	return nil
}

func (r *Reconciler) patchStatus(ctx context.Context, original, pe *v1alpha1.PreviewEnvironment) error {
	if equality.Semantic.DeepEqual(original.Status, pe.Status) {
		return nil
	}
	err := r.Client.Status().Patch(ctx, pe, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	return client.IgnoreNotFound(err)
}

func (r *Reconciler) eventf(pe *v1alpha1.PreviewEnvironment, kind, reason, action, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(pe, nil, kind, reason, action, format, args...)
	}
}

// relevantChange ignores status-only updates (the reconciler's own writes)
// while still seeing spec, deletion, finalizer, label and annotation changes.
var relevantChange = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, n := e.ObjectOld, e.ObjectNew
	return o.GetGeneration() != n.GetGeneration() ||
		o.GetDeletionTimestamp().IsZero() != n.GetDeletionTimestamp().IsZero() ||
		!slices.Equal(o.GetFinalizers(), n.GetFinalizers()) ||
		!maps.Equal(o.GetLabels(), n.GetLabels()) ||
		!maps.Equal(o.GetAnnotations(), n.GetAnnotations())
}}

// Notifier turns operation progress into reconcile requests.
type Notifier struct {
	ch chan event.TypedGenericEvent[*v1alpha1.PreviewEnvironment]
}

// NewNotifier returns a Notifier with a generous buffer.
func NewNotifier() *Notifier {
	return &Notifier{ch: make(chan event.TypedGenericEvent[*v1alpha1.PreviewEnvironment], 4096)}
}

// Notify enqueues a reconcile for name without blocking the caller.
func (n *Notifier) Notify(name types.NamespacedName) {
	ev := event.TypedGenericEvent[*v1alpha1.PreviewEnvironment]{Object: &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}}
	select {
	case n.ch <- ev:
	default:
		go func() { n.ch <- ev }() // never drop a completion; never block an engine step
	}
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, n *Notifier, maxConcurrent int) error {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	if r.Guard == nil {
		r.Guard = Allowed{}
	}
	if r.BaseBackoff <= 0 {
		r.BaseBackoff = 10 * time.Second
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 10 * time.Minute
	}
	if r.GuardRetry <= 0 {
		r.GuardRetry = 15 * time.Second
	}
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	if err := mgr.Add(r.Runner); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("previewenvironment").
		For(&v1alpha1.PreviewEnvironment{}, builder.WithPredicates(relevantChange)).
		WatchesRawSource(source.Channel(n.ch, &handler.TypedEnqueueRequestForObject[*v1alpha1.PreviewEnvironment]{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
