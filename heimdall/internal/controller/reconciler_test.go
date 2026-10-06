//go:build !windows

package controller

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
)

const wait = 20 * time.Second

func TestCreateBecomesReady(t *testing.T) {
	h := newHarness(t, &script{})
	h.create(h.environment("pr7"))
	pe := h.eventually("pr7", "Ready", wait, ready)

	if !controllerutil.ContainsFinalizer(pe, Finalizer) {
		t.Error("finalizer missing")
	}
	for _, c := range []string{v1alpha1.ConditionReady, v1alpha1.ConditionGuardrails, v1alpha1.ConditionDependencies,
		v1alpha1.ConditionBaselineDatabase, v1alpha1.ConditionApplication, v1alpha1.ConditionSmokeTests} {
		if !meta.IsStatusConditionTrue(pe.Status.Conditions, c) {
			t.Errorf("condition %s not True: %+v", c, meta.FindStatusCondition(pe.Status.Conditions, c))
		}
	}
	if meta.IsStatusConditionTrue(pe.Status.Conditions, v1alpha1.ConditionProgressing) {
		t.Error("Progressing should be False when idle")
	}
	if len(pe.Status.URLs) != 1 || !pe.Status.URLs[0].Primary || pe.Status.Namespace != "heimdall-pr7-demo-abcd" {
		t.Errorf("urls/namespace: %+v %s", pe.Status.URLs, pe.Status.Namespace)
	}
	if o := pe.Status.Operation; o == nil || o.Result != v1alpha1.ResultSucceeded || o.Type != v1alpha1.OperationApply {
		t.Errorf("operation: %+v", o)
	}
	if len(pe.Status.Steps) != 4 || pe.Status.Steps[3].Name != "smoke/run" || pe.Status.Steps[3].State != "succeeded" {
		t.Errorf("steps: %+v", pe.Status.Steps)
	}
	if pe.Status.ObservedGeneration != pe.Generation {
		t.Errorf("observedGeneration %d != %d", pe.Status.ObservedGeneration, pe.Generation)
	}
}

func TestSpecChangeRollsOutNewGeneration(t *testing.T) {
	h := newHarness(t, &script{})
	h.create(h.environment("pr7"))
	h.eventually("pr7", "Ready", wait, ready)
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) {
		doc := configDoc("two")
		pe.Spec.Generation, pe.Spec.Config = 2, v1alpha1.ConfigSource{Inline: doc, SHA256: digest(doc)}
	})
	h.eventually("pr7", "generation 2 Ready", wait, ready)
	if n := h.script.count("apply"); n != 2 {
		t.Errorf("apply calls = %d, want 2", n)
	}
}

func TestDeleteDestroysThenReleases(t *testing.T) {
	h := newHarness(t, &script{})
	h.create(h.environment("pr7"))
	h.eventually("pr7", "Ready", wait, ready)
	if err := h.client.Delete(context.Background(), h.get("pr7")); err != nil {
		t.Fatal(err)
	}
	h.gone("pr7", wait)
	if h.script.count("destroy") != 1 {
		t.Errorf("destroy calls = %d", h.script.count("destroy"))
	}
}

func TestDeleteWaitsForASuccessfulDestroy(t *testing.T) {
	failures := 2
	s := &script{destroy: func(context.Context, engine.Spec) (*engine.Result, error) {
		if failures > 0 {
			failures--
			return nil, &engine.Error{Code: "engine.destroy_stuck", Message: "namespace deletion did not finish"}
		}
		return &engine.Result{Phase: "destroyed"}, nil
	}}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	h.eventually("pr7", "Ready", wait, ready)
	if err := h.client.Delete(context.Background(), h.get("pr7")); err != nil {
		t.Fatal(err)
	}
	h.eventually("pr7", "destroy failure recorded", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.LastError != nil && pe.Status.LastError.Code == "engine.destroy_stuck"
	})
	h.gone("pr7", wait)
	if n := h.script.count("destroy"); n != 3 {
		t.Errorf("destroy calls = %d, want 3 (two failures, then success)", n)
	}
}

// A slow generation-1 deployment finishes after generation 2 was requested.
// Its result must not count, and generation 2 must not run concurrently.
func TestStaleGenerationIsFencedOut(t *testing.T) {
	release := make(chan struct{})
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		if spec.Context.Generation == 1 {
			emit(engine.Event{Operation: "apply", Stage: "baseline-db/migrate", State: "running", Generation: 1, At: time.Now()})
			<-release // a Job that ignores cancellation and completes late
			return &engine.Result{Phase: "ready", Generation: 1}, nil
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	h.eventually("pr7", "generation 1 provisioning", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseProvisioning
	})
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) {
		doc := configDoc("two")
		pe.Spec.Generation, pe.Spec.Config = 2, v1alpha1.ConfigSource{Inline: doc, SHA256: digest(doc)}
	})
	time.Sleep(500 * time.Millisecond) // the controller has seen generation 2
	if h.script.count("apply") != 1 {
		t.Fatal("generation 2 started before generation 1 stopped")
	}
	close(release)
	pe := h.eventually("pr7", "generation 2 Ready", wait, ready)
	if pe.Status.DeployedGeneration != 2 || h.script.overlap.Load() {
		t.Fatalf("deployed %d, overlap %v", pe.Status.DeployedGeneration, h.script.overlap.Load())
	}
}

func TestRestartMidOperationResumes(t *testing.T) {
	blocked := make(chan struct{}, 1)
	var first = true
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		if first {
			first = false
			emit(engine.Event{Operation: "apply", Stage: "dependencies/start", State: "running", Generation: spec.Context.Generation, At: time.Now()})
			blocked <- struct{}{}
			<-ctx.Done() // the agent dies in the middle of a stage
			return nil, ctx.Err()
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	<-blocked
	h.eventually("pr7", "running", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Operation != nil && pe.Status.Operation.Result == v1alpha1.ResultRunning
	})
	h.shutdown()
	h.start() // a new agent process
	pe := h.eventually("pr7", "Ready after restart", wait, ready)
	if pe.Status.LastError != nil || h.script.count("apply") != 2 {
		t.Errorf("lastError %+v, apply calls %d", pe.Status.LastError, h.script.count("apply"))
	}
}

func TestRetryableFailureBacksOffThenRecovers(t *testing.T) {
	var mu sync.Mutex
	var started []time.Time
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		mu.Lock()
		started = append(started, time.Now())
		n := len(started)
		mu.Unlock()
		if n < 3 {
			return nil, &engine.Error{Code: "engine.cluster", Message: "the API server is unavailable"}
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	pe := h.eventually("pr7", "Ready after retries", wait, ready)
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 3 {
		t.Fatalf("attempts = %d", len(started))
	}
	// 200ms, then 400ms: exponential backoff, never a hot loop.
	if d1, d2 := started[1].Sub(started[0]), started[2].Sub(started[1]); d1 < 150*time.Millisecond || d2 < 350*time.Millisecond {
		t.Errorf("backoff too short: %s, %s", d1, d2)
	}
	if pe.Status.Operation.Attempts != 0 || pe.Status.LastError != nil {
		t.Errorf("success must clear attempts and error: %+v %+v", pe.Status.Operation, pe.Status.LastError)
	}
}

func TestTerminalFailureWaitsForNewInput(t *testing.T) {
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		if spec.Context.Generation == 1 {
			emit(engine.Event{Operation: "apply", Stage: "baseline-db/migrate", State: "running", Generation: 1, At: time.Now()})
			emit(engine.Event{Operation: "apply", Stage: "baseline-db/migrate", State: "failed", Generation: 1, At: time.Now(), Code: "engine.job_failed"})
			return nil, &engine.Error{Code: "engine.job_failed", Message: "Job heimdall-migrate-g1 failed; inspect its logs"}
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	pe := h.eventually("pr7", "Failed", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseFailed
	})
	le := pe.Status.LastError
	if le == nil || le.Code != "engine.job_failed" || le.Retryable || le.Step != "baseline-db/migrate" {
		t.Fatalf("lastError: %+v", le)
	}
	if c := meta.FindStatusCondition(pe.Status.Conditions, v1alpha1.ConditionBaselineDatabase); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("baseline condition: %+v", c)
	}
	time.Sleep(time.Second)
	if n := h.script.count("apply"); n != 1 {
		t.Fatalf("a terminal failure was retried %d times", n-1)
	}
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) {
		doc := configDoc("fixed")
		pe.Spec.Generation, pe.Spec.Config = 2, v1alpha1.ConfigSource{Inline: doc, SHA256: digest(doc)}
	})
	h.eventually("pr7", "Ready after a fix", wait, ready)
}

func TestResetNonce(t *testing.T) {
	h := newHarness(t, &script{})
	h.create(h.environment("pr7"))
	h.eventually("pr7", "Ready", wait, ready)
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.ResetNonce = 1 })
	pe := h.eventually("pr7", "reset completed", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.CompletedResetNonce == 1 && pe.Status.Phase == v1alpha1.PhaseReady
	})
	if h.script.count("reset") != 1 || pe.Status.Operation.Type != v1alpha1.OperationReset {
		t.Errorf("reset calls %d, operation %+v", h.script.count("reset"), pe.Status.Operation)
	}
}

func TestDesiredStateDestroyedAndBack(t *testing.T) {
	h := newHarness(t, &script{})
	h.create(h.environment("pr7"))
	h.eventually("pr7", "Ready", wait, ready)
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.DesiredState = v1alpha1.DesiredDestroyed })
	h.eventually("pr7", "Destroyed", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseDestroyed
	})
	time.Sleep(500 * time.Millisecond)
	if h.script.count("destroy") != 1 {
		t.Fatalf("destroy calls = %d", h.script.count("destroy"))
	}
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.DesiredState = v1alpha1.DesiredRunning })
	h.eventually("pr7", "Ready again", wait, ready)
}

// A lock held by someone else (an agent killed mid-operation) is waited out
// at a steady pace, and the environment never shows as Failed meanwhile.
func TestBusyEnvironmentWaitsWithoutFailing(t *testing.T) {
	var mu sync.Mutex
	var started []time.Time
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		mu.Lock()
		started = append(started, time.Now())
		n := len(started)
		mu.Unlock()
		if n < 4 {
			return nil, &engine.Error{Code: CodeBusy, Message: "another operation is active for this environment"}
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	busy := h.eventually("pr7", "waiting", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.LastError != nil && pe.Status.LastError.Code == CodeBusy
	})
	if busy.Status.Phase == v1alpha1.PhaseFailed || busy.Status.Operation.Result != v1alpha1.ResultInterrupted || busy.Status.Operation.Attempts != 0 {
		t.Errorf("a held lock is not a failure: phase %s, operation %+v", busy.Status.Phase, busy.Status.Operation)
	}
	if c := meta.FindStatusCondition(busy.Status.Conditions, v1alpha1.ConditionProgressing); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("waiting is progress: %+v", c)
	}
	h.eventually("pr7", "Ready once the lock is free", wait, ready)
	mu.Lock()
	defer mu.Unlock()
	// The base backoff (200ms, rounded up to status's whole seconds) every
	// time: not exponential, and attempts are not counted.
	for i := 1; i < len(started); i++ {
		if d := started[i].Sub(started[i-1]); d < 150*time.Millisecond || d > 1500*time.Millisecond {
			t.Errorf("retry %d after %v", i, d)
		}
	}
}

// The agent restarting in the middle of a repair (a Degraded environment
// re-applying the same generation) resumes the repair.
func TestRestartDuringRepairResumes(t *testing.T) {
	blocked := make(chan struct{}, 1)
	var degraded atomic.Bool
	degraded.Store(true)
	s := &script{status: func(engine.Spec) (*engine.Status, error) {
		if degraded.CompareAndSwap(true, false) {
			return &engine.Status{Objects: []engine.ObjectStatus{{Kind: "Deployment", Name: "app", Ready: false}}}, nil
		}
		return &engine.Status{Result: engine.Result{Phase: "ready"}}, nil
	}}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		if s.count("apply") == 2 { // the repair
			emit(engine.Event{Operation: "apply", Stage: "application/wave-1", State: "running", Generation: spec.Context.Generation, At: time.Now()})
			blocked <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return readyApply(ctx, spec, emit)
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	<-blocked
	h.eventually("pr7", "repair running", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Operation != nil && pe.Status.Operation.Result == v1alpha1.ResultRunning
	})
	h.shutdown()
	h.start()
	h.eventually("pr7", "Ready after the resumed repair", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return ready(pe) && s.count("apply") == 3
	})
}

func TestDegradedEnvironmentSelfHeals(t *testing.T) {
	broken := true
	s := &script{status: func(engine.Spec) (*engine.Status, error) {
		if broken {
			broken = false
			return &engine.Status{Objects: []engine.ObjectStatus{{Kind: "Deployment", Name: "app", Ready: false}}}, nil
		}
		return &engine.Status{Result: engine.Result{Phase: "ready"}}, nil
	}}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	h.eventually("pr7", "re-applied after drift", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return ready(pe) && s.count("apply") == 2
	})
}

func TestGuardBlocksChangesUntilPolicyIsEnforced(t *testing.T) {
	h := newHarness(t, &script{})
	h.guard.ok.Store(false)
	h.create(h.environment("pr7"))
	time.Sleep(time.Second)
	if h.script.count("apply") != 0 {
		t.Fatal("the agent acted before its admission policy was enforced")
	}
	h.guard.ok.Store(true)
	h.eventually("pr7", "Ready once allowed", 30*time.Second, ready)
}

func TestInvalidSpecIsRejectedWithoutEngineCalls(t *testing.T) {
	h := newHarness(t, &script{})
	pe := h.environment("pr7")
	pe.Spec.Config.SHA256 = digest("something else")
	h.create(pe)
	got := h.eventually("pr7", "Failed", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.LastError != nil
	})
	if got.Status.LastError.Code != CodeConfigDigest || got.Status.LastError.Retryable {
		t.Errorf("lastError: %+v", got.Status.LastError)
	}
	if len(got.Status.Diagnoses) == 0 || got.Status.Diagnoses[0].Code != string(diagnose.ConfigInvalid) {
		t.Errorf("diagnoses: %+v", got.Status.Diagnoses)
	}
	if h.script.count("apply") != 0 {
		t.Error("engine was called for an invalid spec")
	}
}

// A failed operation is diagnosed: ranked findings in status, the root cause
// in the Ready condition and an event; a later success clears them.
func TestFailureIsDiagnosed(t *testing.T) {
	var seen diagnose.Failure
	s := &script{}
	s.apply = func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
		if spec.Context.Generation == 1 {
			emit(engine.Event{Operation: "apply", Stage: "baseline-db/migrate", State: "failed", Code: "engine.job_failed",
				Generation: 1, At: time.Now()})
			return nil, &engine.Error{Code: "engine.job_failed", Message: "Job heimdall-migrate-g1 failed; inspect its logs"}
		}
		return readyApply(ctx, spec, emit)
	}
	s.diagnose = func(_ engine.Spec, f diagnose.Failure) (*diagnose.Report, error) {
		seen = f
		return &diagnose.Report{Diagnoses: []diagnose.Diagnosis{
			{Code: diagnose.MigrationFailed, Summary: `Migration failed: column "owner_id" of relation "catalog" contains null values (SQLSTATE 23502)`,
				Subject: "job/heimdall-migrate-g1", Stage: "baseline-db", Suggestion: "Give the column a DEFAULT.",
				Evidence: []string{"error: column \"owner_id\" of relation \"catalog\" contains null values", "code: '23502'"}},
			{Code: diagnose.HealthcheckFailed, Summary: "api fails its readiness check", Subject: "deployment/api",
				Suggestion: "Fix health.path.", Evidence: []string{"not kept for later findings"}},
		}}, nil
	}
	h := newHarness(t, s)
	h.create(h.environment("pr7"))
	pe := h.eventually("pr7", "Failed with a diagnosis", wait, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseFailed && len(pe.Status.Diagnoses) > 0
	})
	if seen.Code != "engine.job_failed" {
		t.Errorf("the diagnoser got %+v", seen)
	}
	d := pe.Status.Diagnoses
	if len(d) != 2 || d[0].Code != "MIGRATION_FAILED" || d[0].Subject != "job/heimdall-migrate-g1" || len(d[0].Evidence) != 2 || len(d[1].Evidence) != 0 {
		t.Errorf("diagnoses: %+v", d)
	}
	if c := meta.FindStatusCondition(pe.Status.Conditions, v1alpha1.ConditionReady); c == nil || !strings.HasPrefix(c.Message, "MIGRATION_FAILED: Migration failed") {
		t.Errorf("Ready condition: %+v", c)
	}
	h.update("pr7", func(pe *v1alpha1.PreviewEnvironment) {
		doc := configDoc("two")
		pe.Spec.Generation, pe.Spec.Config = 2, v1alpha1.ConfigSource{Inline: doc, SHA256: digest(doc)}
	})
	pe = h.eventually("pr7", "Ready at generation 2", wait, ready)
	if len(pe.Status.Diagnoses) != 0 {
		t.Errorf("diagnoses outlived the failure: %+v", pe.Status.Diagnoses)
	}
}
