package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
)

// A snapshot captured from the real failure on kind (test/e2e/diagnose).
const migrationSnapshot = "../diagnose/testdata/scenarios/migration-not-null/snapshot.json"

func TestDiagnose_SnapshotFormats(t *testing.T) {
	code, out, errs := run("diagnose", "--snapshot", migrationSnapshot)
	if code != ExitInvalid || !strings.HasPrefix(out, "MIGRATION_FAILED: Database migration failed") {
		t.Fatalf("text: exit %d\n%s%s", code, out, errs)
	}
	code, out, _ = run("diagnose", "--snapshot", migrationSnapshot, "--format", "json")
	var r diagnose.Report
	if code != ExitInvalid || json.Unmarshal([]byte(out), &r) != nil || r.RootCause().SQL.State != "23502" {
		t.Fatalf("json: exit %d\n%s", code, out)
	}
	code, out, _ = run("diagnose", "--snapshot", migrationSnapshot, "--format", "markdown")
	if code != ExitInvalid || !strings.HasPrefix(out, "### Preview failed: Database migration failed") {
		t.Fatalf("markdown: exit %d\n%s", code, out)
	}
}

func TestDiagnose_HealthyAndErrors(t *testing.T) {
	dir := t.TempDir()
	healthy := filepath.Join(dir, "healthy.json")
	b, _ := json.Marshal(diagnose.Snapshot{Version: diagnose.SnapshotVersion, Namespace: "heimdall-pr1-demo-abcd"})
	if err := os.WriteFile(healthy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("diagnose", "--snapshot", healthy); code != ExitOK || out != "No problems found.\n" {
		t.Errorf("healthy: exit %d %q", code, out)
	}
	future := filepath.Join(dir, "future.json")
	if err := os.WriteFile(future, []byte(`{"version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"diagnose", "--snapshot", future}, "version 99 is not supported"},
		{[]string{"diagnose", "--snapshot", filepath.Join(dir, "missing.json")}, "missing.json"},
		{[]string{"diagnose", "--snapshot", healthy, "--format", "yaml"}, "Usage: heimdall diagnose"},
		{[]string{"diagnose", "--snapshot", healthy, "unexpected"}, "Usage: heimdall diagnose"},
		{[]string{"diagnose", "--state", filepath.Join(dir, "none.json")}, "none.json"},
	} {
		if code, _, errs := run(tc.args...); code != ExitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, errs)
		}
	}
}

// --save-snapshot round-trips: what is saved diagnoses the same.
func TestDiagnose_SaveSnapshot(t *testing.T) {
	saved := filepath.Join(t.TempDir(), "copy.json")
	_, first, _ := run("diagnose", "--snapshot", migrationSnapshot, "--save-snapshot", saved, "--format", "json")
	_, second, _ := run("diagnose", "--snapshot", saved, "--format", "json")
	if first == "" || first != second {
		t.Errorf("round trip differs:\n%s\n%s", first, second)
	}
}

func TestDiagnoseSanitizesAnOfflineSnapshot(t *testing.T) {
	dir := t.TempDir()
	source, saved := filepath.Join(dir, "raw.json"), filepath.Join(dir, "saved.json")
	snapshot := diagnose.Snapshot{Version: diagnose.SnapshotVersion, Namespace: "heimdall-pr1-demo-abcd",
		Failure: &diagnose.Failure{Code: "engine.workload_failed", Message: "postgres://app:rawPassword123@postgres:5432/app"},
		Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "api", Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "rawEnvironmentValue"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "PRIVATE", Value: "rawEnvironmentValue"}}}}}}}}
	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("diagnose", "--snapshot", source, "--save-snapshot", saved, "--format", "json")
	if code != ExitInvalid {
		t.Fatalf("exit %d: %s", code, errs)
	}
	b, err = os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{out, string(b)} {
		if strings.Contains(text, "rawPassword123") || strings.Contains(text, "rawEnvironmentValue") || strings.Contains(text, "last-applied") {
			t.Fatalf("offline snapshot leaked sensitive input: %s", text)
		}
	}
	if !strings.Contains(out, "@postgres:5432/app") {
		t.Fatal("connection host was lost from diagnosis")
	}
}
