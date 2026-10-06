package diagnose

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var update = flag.Bool("update", false, "rewrite the golden files of captured scenarios")

// expected root cause of each captured scenario (test/e2e/diagnose).
var expected = map[string]Code{
	"image-pull":          ImagePullFailed,
	"migration-not-null":  MigrationFailed,
	"out-of-memory":       OutOfMemory,
	"crash-loop":          ContainerCrash,
	"smoke-test":          SmokeTestFailed,
	"health-check":        HealthcheckFailed,
	"quota":               QuotaExceeded,
	"route-rejected":      NoEndpoints,
	"no-capacity":         NoCapacity,
	"database-down":       DBUnreachable,
	"import-failed":       SeedFailed,
	"import-not-approved": PolicyDenied,
	"missing-secret":      ConfigInvalid,
	"stale-generation":    StaleGeneration,
	"step-timeout":        Unclassified,
}

func loadScenarios(t *testing.T) map[string]*Snapshot {
	t.Helper()
	dirs, err := filepath.Glob("testdata/scenarios/*")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*Snapshot{}
	for _, dir := range dirs {
		b, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s Snapshot
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		out[filepath.Base(dir)] = &s
	}
	for name := range expected {
		if out[name] == nil {
			t.Errorf("no captured fixture for %s: run HEIMDALL_E2E_CAPTURE=1 test/e2e/diagnose/run.sh", name)
		}
	}
	return out
}

// Each rule against snapshots captured from real failures on kind: the
// expected root cause, and exact output (JSON, PR comment, CLI text).
func TestCapturedScenarios(t *testing.T) {
	for name, s := range loadScenarios(t) {
		t.Run(name, func(t *testing.T) {
			if s.Version != SnapshotVersion {
				t.Fatalf("snapshot version %d", s.Version)
			}
			r := Diagnose(s)
			if root := r.RootCause(); root == nil || root.Code != expected[name] {
				t.Fatalf("root cause %+v, want %s", root, expected[name])
			}
			var js, md, text bytes.Buffer
			if err := WriteJSON(&js, r); err != nil {
				t.Fatal(err)
			}
			if err := WriteMarkdown(&md, r, CommentContext{Environment: s.Namespace, Generation: s.Generation}); err != nil {
				t.Fatal(err)
			}
			if err := WriteText(&text, r); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("testdata", "scenarios", name)
			golden(t, filepath.Join(dir, "report.golden.json"), js.Bytes())
			golden(t, filepath.Join(dir, "comment.golden.md"), md.Bytes())
			golden(t, filepath.Join(dir, "text.golden"), text.Bytes())
		})
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run with -update after reviewing):\n--- got ---\n%s", path, got)
	}
}

// Noise immunity: what happened to other generations, routine events and
// pods on their way out must not change the diagnosis at all.
func TestUnrelatedNoiseChangesNothing(t *testing.T) {
	for name, s := range loadScenarios(t) {
		t.Run(name, func(t *testing.T) {
			want := Diagnose(s)
			got := Diagnose(withNoise(s))
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("noise changed the report:\n--- with noise ---\n%s\n--- without ---\n%s", g, w)
			}
		})
	}
}

func withNoise(orig *Snapshot) *Snapshot {
	b, _ := json.Marshal(orig)
	var s Snapshot
	_ = json.Unmarshal(b, &s)
	old := strconv.FormatInt(s.Generation-1, 10)
	now := metav1.NewTime(t0)
	// Routine events for every pod.
	for _, p := range orig.Pods {
		for _, reason := range []string{"Scheduled", "Pulled", "Created", "Started"} {
			s.Events = append(s.Events, corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: p.Name + "." + reason}, Type: corev1.EventTypeNormal,
				Reason: reason, Message: reason + " " + p.Name, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: p.Name}, LastTimestamp: now})
		}
	}
	// A previous generation's failed migration, with its pod, logs and events.
	labels := map[string]string{labelName: "heimdall-migrate", labelComponent: "migration", "heimdall.dev/generation": old,
		"heimdall.dev/stage": "baseline-db"}
	s.Jobs = append(s.Jobs, batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-migrate-g" + old, Labels: labels},
		Spec:   batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}}})
	oldPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-migrate-g" + old + "-abcde", Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "heimdall-migrate-g" + old}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}}}}
	s.Pods = append(s.Pods, oldPod)
	s.Logs = append(s.Logs, Log{Pod: oldPod.Name, Container: "main", Text: `error: relation "orders" already exists` + "\n  code: '42P07'"})
	s.Events = append(s.Events, corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: oldPod.Name + ".BackOff"}, Type: corev1.EventTypeWarning,
		Reason: "BackOff", Message: "Back-off restarting failed container", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: oldPod.Name}})
	// A pod of a previous rollout, terminating, crash-looping.
	gone := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0ld-xyz", DeletionTimestamp: &now,
		Labels:          map[string]string{labelName: "api", labelComponent: "service"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-0ld"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 7,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}}}}
	s.Pods = append(s.Pods, gone)
	// Warnings about objects that no longer exist.
	s.Events = append(s.Events, corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ghost.Unhealthy"}, Type: corev1.EventTypeWarning,
		Reason: "Unhealthy", Message: "Readiness probe failed: HTTP probe failed with statuscode: 500",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-gone-123"}})
	return &s
}
