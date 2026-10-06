package controller

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// stages maps engine step names to stage conditions, in pipeline order.
// Apply steps are "<stage>/<step>"; reset steps have their own names.
var stages = []struct {
	condition string
	prefixes  []string
}{
	{v1alpha1.ConditionGuardrails, []string{string(render.StageGuardrails)}},
	{v1alpha1.ConditionDependencies, []string{string(render.StageDependencies), "cache-and-broker"}},
	{v1alpha1.ConditionBaselineDatabase, []string{string(render.StageBaselineDB), "database"}},
	{v1alpha1.ConditionApplication, []string{string(render.StageApplication), "resume"}},
	{v1alpha1.ConditionSmokeTests, []string{string(render.StageSmoke), "smoke"}},
}

// stageOf returns the stage index of an engine step, or -1 for steps that
// belong to no stage (quiesce, storage, prune, ...).
func stageOf(step string) int {
	head, _, _ := strings.Cut(step, "/")
	for i, s := range stages {
		for _, p := range s.prefixes {
			if head == p {
				return i
			}
		}
	}
	return -1
}

// stepStatuses folds the event stream into one entry per step, in order.
func stepStatuses(events []engine.Event) []v1alpha1.StepStatus {
	var out []v1alpha1.StepStatus
	index := map[string]int{}
	for _, ev := range events {
		if ev.Stage == "complete" {
			continue
		}
		i, ok := index[ev.Stage]
		if !ok {
			i = len(out)
			index[ev.Stage] = i
			out = append(out, v1alpha1.StepStatus{Name: ev.Stage})
		}
		s := &out[i]
		s.State = ev.State
		s.Code = ev.Code
		if ev.State == "running" {
			at := metav1.NewTime(ev.At)
			s.StartedAt = &at
			s.DurationSeconds = 0
		} else {
			s.DurationSeconds = int64(ev.Duration.Seconds())
		}
	}
	if len(out) > maxSteps {
		out = out[len(out)-maxSteps:]
	}
	return out
}

// setStageConditions derives the per-stage conditions from an operation's
// steps. A stage is done once a later stage has started, or when the whole
// operation succeeded.
func setStageConditions(st *v1alpha1.PreviewEnvironmentStatus, generation int64, steps []v1alpha1.StepStatus, succeeded bool) {
	latest := -1
	failed := map[int]string{}
	running := map[int]bool{}
	for _, s := range steps {
		i := stageOf(s.Name)
		if i < 0 {
			continue
		}
		latest = max(latest, i)
		switch s.State {
		case "failed":
			failed[i] = s.Code
		case "running":
			running[i] = true
		}
	}
	for i, s := range stages {
		c := metav1.Condition{Type: s.condition, ObservedGeneration: generation}
		switch {
		case failed[i] != "":
			c.Status, c.Reason, c.Message = metav1.ConditionFalse, "StepFailed", failed[i]
		case succeeded || i < latest:
			c.Status, c.Reason, c.Message = metav1.ConditionTrue, "Succeeded", ""
		case running[i] || i == latest:
			c.Status, c.Reason, c.Message = metav1.ConditionUnknown, "InProgress", ""
		default:
			c.Status, c.Reason, c.Message = metav1.ConditionUnknown, "Pending", ""
		}
		meta.SetStatusCondition(&st.Conditions, c)
	}
}

// setSummaryConditions keeps Ready and Progressing consistent with Phase.
func setSummaryConditions(st *v1alpha1.PreviewEnvironmentStatus, generation int64, progressing bool) {
	ready := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: string(st.Phase), ObservedGeneration: generation}
	if st.Phase == v1alpha1.PhaseReady {
		ready.Status, ready.Reason = metav1.ConditionTrue, "Ready"
	}
	if st.LastError != nil && st.Phase != v1alpha1.PhaseReady {
		ready.Message = st.LastError.Code + ": " + st.LastError.Message
		// The diagnosis says it better, when there is one.
		if st.Phase == v1alpha1.PhaseFailed && len(st.Diagnoses) > 0 {
			ready.Message = st.Diagnoses[0].Code + ": " + st.Diagnoses[0].Summary
		}
	}
	if ready.Reason == "" {
		ready.Reason = string(v1alpha1.PhasePending)
	}
	meta.SetStatusCondition(&st.Conditions, ready)

	p := metav1.Condition{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: "Idle", ObservedGeneration: generation}
	if progressing && st.Operation != nil {
		p.Status, p.Reason = metav1.ConditionTrue, string(st.Operation.Type)
	}
	meta.SetStatusCondition(&st.Conditions, p)
}

// urls converts the engine's result URLs.
func urls(in []render.URL) []v1alpha1.URL {
	out := make([]v1alpha1.URL, 0, len(in))
	for _, u := range in {
		out = append(out, v1alpha1.URL{Service: u.Service, URL: u.URL, Primary: u.Primary})
	}
	return out
}
