package diagnose

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/heimdall-dev/heimdall/internal/render"
)

const ns = "heimdall-pr7-demo-abcd"

// planted are secret values a preview could leak into what a snapshot
// captures: its generated credentials and a tenant secret.
var planted = []string{"Zq8v2Lk9Wm4Xp7RtUu3Ny6Hb1D", "sk_live_51HxTenantSecretValue"}

func preview(extra ...runtime.Object) *fake.Clientset {
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: render.CredentialsSecret, Namespace: ns},
			Data: map[string][]byte{"POSTGRES_PASSWORD": []byte(planted[0])}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: render.AppSecretsSecret, Namespace: ns},
			Data: map[string][]byte{"STRIPE_KEY": []byte(planted[1])}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: journalName, Namespace: ns}, Data: map[string]string{"record": `{
			"generation": 3, "operation": "apply",
			"events": [{"stage": "baseline-db/migrate", "state": "running"},
			           {"stage": "baseline-db/migrate", "state": "failed", "code": "engine.job_failed"}]}`}},
	}
	return fake.NewClientset(append(objs, extra...)...)
}

// Every place a secret could reach a snapshot: env values, args, events,
// termination and condition messages. None may survive Collect.
func TestCollectRedactsPlantedSecrets(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: ns,
		Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": planted[1]},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "heimdall-engine"}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app",
			Env:  []corev1.EnvVar{{Name: "PAYMENT_KEY", Value: planted[1]}, {Name: "MODE", Value: "preview"}},
			Args: []string{"--db-password=" + planted[0]}}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 2,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1,
				Message: "connecting to postgres://app:" + planted[0] + "@postgres:5432/app"}}}}},
	}
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "api-1.1", Namespace: ns}, Type: corev1.EventTypeWarning, Reason: "BackOff",
		Message: "token " + base64.StdEncoding.EncodeToString([]byte(planted[0])), InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"}}
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-migrate-g3", Namespace: ns}, Spec: batchv1.JobSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main",
			Command: []string{"sh", "-c", "PGPASSWORD=" + planted[0] + " psql"}}}}}}}

	s, err := Collect(context.Background(), preview(p, ev, j), Options{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	for _, secret := range append(planted, base64.StdEncoding.EncodeToString([]byte(planted[0]))) {
		if strings.Contains(string(b), secret) {
			t.Errorf("planted secret %q is in the snapshot", secret)
		}
	}
	for _, keep := range []string{`"name":"PAYMENT_KEY"`, "CrashLoopBackOff", "@postgres:5432/app"} {
		if !strings.Contains(string(b), keep) {
			t.Errorf("snapshot lost %s", keep)
		}
	}
	if strings.Contains(string(b), "managedFields") || strings.Contains(string(b), "last-applied") {
		t.Error("managed fields or last-applied annotations kept")
	}
	// From the journal: the generation and the step that failed.
	if s.Generation != 3 || s.Failure == nil || s.Failure.Code != "engine.job_failed" || s.Failure.Step != "baseline-db/migrate" {
		t.Errorf("journal: generation %d failure %+v", s.Generation, s.Failure)
	}
	// The crashed container's previous log was requested (the fake answers
	// with a fixed text).
	if len(s.Logs) != 1 || !s.Logs[0].Previous || s.Logs[0].Container != "app" {
		t.Errorf("logs: %+v", s.Logs)
	}
}

// Without the injected values, logs cannot be redacted reliably: none are
// collected.
func TestCollectOmitsLogsWithoutSecretValues(t *testing.T) {
	c := preview(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: ns},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}}})
	c.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", nil)
	})
	s, err := Collect(context.Background(), c, Options{Namespace: ns, Failure: &Failure{Code: "engine.workload_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Logs) != 0 || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "logs omitted") {
		t.Errorf("logs %+v notes %v", s.Logs, s.Notes)
	}
	if s.Failure.Code != "engine.workload_failed" {
		t.Errorf("a given failure must win over the journal: %+v", s.Failure)
	}
}

func TestCollectNeedsTheNamespace(t *testing.T) {
	if _, err := Collect(context.Background(), fake.NewClientset(), Options{Namespace: ns}); err == nil {
		t.Error("collected a namespace that does not exist")
	}
}

// An error printed before a long dump (node-postgres prints the whole client
// object) must survive the cap.
func TestCondenseKeepsTheErrorBeforeADump(t *testing.T) {
	lines := []string{"{\"event\":\"api.listening\"}", "Error: Connection terminated unexpectedly", "    at Connection.<anonymous> (client.js:132:73)"}
	for i := range 200 {
		lines = append(lines, "    _field"+strconv.Itoa(i)+": null,")
	}
	lines = append(lines, "Node.js v24.21.0")
	got := condense(strings.Join(lines, "\n"), 60, 16<<10)
	out := strings.Split(got, "\n")
	if len(out) > 60 || out[0] != "Error: Connection terminated unexpectedly" || !strings.HasPrefix(out[1], "[... ") ||
		out[len(out)-1] != "Node.js v24.21.0" {
		t.Errorf("condensed (%d lines):\n%s", len(out), got)
	}
	if short := "a\nb\nc"; condense(short+"\n", 60, 16<<10) != short {
		t.Error("a short log must pass through")
	}
	big := condense(strings.Repeat("x", 300)+"\n"+strings.Repeat("y\n", 500), 60, 1024)
	if len(big) > 1024 {
		t.Errorf("byte budget exceeded: %d", len(big))
	}
}

func TestErrorLines(t *testing.T) {
	for line, want := range map[string]bool{
		"Error: Connection terminated unexpectedly":                           true,
		"TypeError: Cannot read properties of undefined (reading 'id')":       true,
		`error: column "owner_id" of relation "catalog" contains null values`: true,
		"FATAL:  data directory has invalid permissions":                      true,
		"panic: runtime error: invalid memory address":                        true,
		"Traceback (most recent call last):":                                  true,
		"    error: [Function: bound _handleErrorEvent],":                     false,
		"  errorCount: 3,": false,
		"    at Connection.<anonymous> (client.js:132:73)": false,
		"errors are retried":  false,
		"  error: undefined,": false,
	} {
		if got := isErrorLine(line); got != want {
			t.Errorf("isErrorLine(%q) = %v", line, got)
		}
	}
}

// A recent journal retains old failed steps even after a retry or a later
// generation succeeds. Live diagnosis must describe its latest work only.
func TestJournalIgnoresSupersededFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		events     string
		generation int64
		failure    *Failure
		wantStep   string
	}{
		{"same generation recovered", `[{"generation":3,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"},{"generation":3,"stage":"complete","state":"ready"}]`, 3, nil, ""},
		{"new generation recovered", `[{"generation":2,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"},{"generation":3,"stage":"complete","state":"ready"}]`, 3, nil, ""},
		{"retry running", `[{"generation":3,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"},{"generation":3,"stage":"baseline-db/migrate","state":"running"}]`, 3, nil, ""},
		{"only historical failure", `[{"generation":2,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"}]`, 3, nil, ""},
		{"historical step cannot fill current failure", `[{"generation":2,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"}]`, 3, &Failure{Code: "engine.job_failed"}, ""},
		{"current failure", `[{"generation":3,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"}]`, 3, nil, "baseline-db/migrate"},
		{"legacy event", `[{"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"}]`, 0, nil, "baseline-db/migrate"},
		{"explicit failure retains step", `[{"generation":3,"stage":"baseline-db/migrate","state":"failed","code":"engine.job_failed"}]`, 3, &Failure{Code: "engine.job_failed", Step: "smoke/run"}, "smoke/run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := preview()
			cm, err := c.CoreV1().ConfigMaps(ns).Get(context.Background(), journalName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cm.Data["record"] = `{"generation":3,"events":` + tc.events + `}`
			if _, err := c.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			s := &Snapshot{Generation: tc.generation, Namespace: ns, Failure: tc.failure}
			if err := readJournal(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			if s.AcceptedGeneration != 3 || s.Generation != 3 {
				t.Fatalf("generations: %+v", s)
			}
			if tc.wantStep != "" {
				if s.Failure == nil || s.Failure.Step != tc.wantStep {
					t.Fatalf("failure: %+v", s.Failure)
				}
			} else if tc.failure == nil && s.Failure != nil || s.Failure != nil && s.Failure.Step != "" {
				t.Fatalf("historical failure imported: %+v", s.Failure)
			}
		})
	}
}

func TestCollectCurrentLogsHaveTheBudget(t *testing.T) {
	pod := func(name, generation string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{render.LabelGeneration: generation}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: lastExit("Error", 1)}}}}
	}
	s, err := Collect(context.Background(), preview(pod("a-old", "2"), pod("z-current", "3")), Options{Namespace: ns, MaxLogs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Logs) != 1 || s.Logs[0].Pod != "z-current" {
		t.Fatalf("logs selected from history: %+v", s.Logs)
	}
}

// Failed Job containers commonly exit without restarting. Read the start of
// their output too, where SQL clients print the error before a long dump.
func TestFailedContainerLogsKeepTheirFirstError(t *testing.T) {
	log := "error: syntax error at or near CRAETE\n" + strings.Repeat("client object dump\n", 300) + "client exited\n"
	var head, tail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/namespaces/"+ns+"/pods/migrate/log" {
			t.Errorf("unexpected log path: %s", req.URL.Path)
		}
		limit, _ := strconv.Atoi(req.URL.Query().Get("limitBytes"))
		out := log
		if req.URL.Query().Get("tailLines") != "" {
			tail = true
			out = "client exited\n"
		} else {
			head = true
			out = out[:limit]
		}
		_, _ = fmt.Fprint(w, out)
	}))
	defer server.Close()
	c, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{Namespace: ns, Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "migrate"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", State: lastExit("Error", 1)}}}}}}
	logs := readLogs(context.Background(), c, s, Options{TailLines: 5, MaxLogBytes: 256, MaxLogs: 1}, nil)
	if !head || !tail || len(logs) != 1 || !strings.HasPrefix(logs[0].Text, "error: syntax error") || !strings.Contains(logs[0].Text, "client exited") {
		t.Fatalf("head %v tail %v logs %+v", head, tail, logs)
	}
}

func TestCollectRejectsNegativeLimits(t *testing.T) {
	for _, o := range []Options{{TailLines: -1}, {MaxLogBytes: -1}, {MaxLogs: -1}, {MaxEvents: -1}} {
		o.Namespace = ns
		if _, err := Collect(context.Background(), preview(), o); err == nil {
			t.Fatalf("accepted negative limit: %+v", o)
		}
	}
}

func TestCondenseHonoursSmallLimitsAndUTF8(t *testing.T) {
	text := strings.Repeat("Error: "+strings.Repeat("界", 200)+"\n", 6) + strings.Repeat("tail\n", 100)
	for _, maxLines := range []int{1, 2, 3, 60} {
		for _, maxBytes := range []int{1, 63, 65, 128, 1024} {
			out := condense(text, maxLines, maxBytes)
			if len(out) > maxBytes || len(strings.Split(out, "\n")) > maxLines || !utf8.ValidString(out) {
				t.Errorf("limits %d/%d: %d bytes, %d lines, valid UTF8 %v", maxLines, maxBytes, len(out), len(strings.Split(out, "\n")), utf8.ValidString(out))
			}
		}
	}
}

func TestCollectReferencedSecrets(t *testing.T) {
	const name, value = "external-secret", "opaqueExternalSecretValue387"
	ref := func() *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "key"}
	}
	for _, tc := range []struct {
		name string
		spec corev1.PodSpec
	}{
		{"env", corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "EXTERNAL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref()}}}}}}},
		{"envFrom", corev1.PodSpec{Containers: []corev1.Container{{Name: "app", EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}}}}},
		{"init", corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", Env: []corev1.EnvVar{{Name: "EXTERNAL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref()}}}}}}},
		{"ephemeral", corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Env: []corev1.EnvVar{{Name: "EXTERNAL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref()}}}}}}}},
		{"volume", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}}}}}},
		{"projected", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "projected", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns}, Spec: tc.spec,
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: lastExit("Error", 1)}}}}
			event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "error", Namespace: ns}, Type: corev1.EventTypeWarning, Message: value}
			c := preview(p, event, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string][]byte{"key": []byte(value)}})
			s, err := Collect(context.Background(), c, Options{Namespace: ns})
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(s)
			if strings.Contains(string(b), value) || len(s.Logs) != 1 {
				t.Fatalf("referenced secret leaked or logs missing: %s", b)
			}
			// A running container can retain a secret deleted after startup.
			if err := c.CoreV1().Secrets(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			s, err = Collect(context.Background(), c, Options{Namespace: ns})
			if err != nil {
				t.Fatal(err)
			}
			b, _ = json.Marshal(s)
			if len(s.Logs) != 0 || strings.Contains(string(b), value) || len(s.Notes) == 0 {
				t.Fatalf("did not fail closed after referenced secret disappeared: %s", b)
			}
		})
	}
}

func TestCollectSecretFailureOmitsAllFreeFormEvidence(t *testing.T) {
	const secret = "unknownInjectedValueOfSecret"
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns, Annotations: map[string]string{"app-note": secret}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Args: []string{secret}}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: secret}}}}}}
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "error", Namespace: ns}, Type: corev1.EventTypeWarning, Reason: "BackOff", Message: secret}
	c := preview(p, event)
	c.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", nil)
	})
	s, err := Collect(context.Background(), c, Options{Namespace: ns, Failure: &Failure{Code: "engine.workload_failed", Message: secret}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), secret) || len(s.Logs) != 0 {
		t.Fatalf("unreadable secret leaked through free-form evidence: %s", b)
	}
	if s.Pods[0].Status.ContainerStatuses[0].State.Terminated.Reason != "Error" || s.Events[0].Reason != "BackOff" || s.Failure.Code != "engine.workload_failed" {
		t.Fatal("structural diagnostic reasons were lost")
	}
}

func TestCollectEventCapKeepsCurrentWarnings(t *testing.T) {
	objects := []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: ns,
			Labels: map[string]string{render.LabelGeneration: "3", labelName: "api", labelComponent: "service"}}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "current-quota", Namespace: ns}, Type: corev1.EventTypeWarning, Reason: "FailedCreate",
			InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "api"}, LastTimestamp: metav1.NewTime(t0),
			Message: `exceeded quota: heimdall-quota, requested: limits.cpu=500m, used: limits.cpu=500m, limited: limits.cpu=500m`},
	}
	for i := range 400 {
		kind := corev1.EventTypeNormal
		if i%2 == 0 {
			kind = corev1.EventTypeWarning // deleted historical objects are noise too
		}
		objects = append(objects, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("noise-%03d", i), Namespace: ns},
			Type: kind, Reason: "Pulled", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "deleted-pod"},
			LastTimestamp: metav1.NewTime(t0.Add(time.Duration(i+1) * time.Second)), Message: "routine or historical event"})
	}
	s, err := Collect(context.Background(), preview(objects...), Options{Namespace: ns, MaxEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "current-quota" {
		t.Fatalf("current warning was evicted: %+v", s.Events)
	}
	if root := Diagnose(s).RootCause(); root == nil || root.Code != QuotaExceeded {
		t.Fatalf("event cap changed root cause: %+v", root)
	}
}

func TestCollectDiagnosesConfirmedMissingTenantSecret(t *testing.T) {
	p := jobPod("heimdall-migrate-g3-x", "heimdall-migrate-g3", "heimdall-migrate", "migration", "3", 0)
	p.Namespace = ns
	p.Spec.Containers = []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "PAYMENTS_API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: render.AppSecretsSecret}, Key: "PAYMENTS_API_KEY"}}}}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError",
			Message: `secret "heimdall-app-secrets" not found; app output could contain an unknownInjectedValue`}}}}
	j := failedJob("heimdall-migrate-g3", "heimdall-migrate", "migration", "3")
	j.Namespace, j.Status = ns, batchv1.JobStatus{Active: 1}
	c := preview(&p, &j)
	if err := c.CoreV1().Secrets(ns).Delete(context.Background(), render.AppSecretsSecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err := Collect(context.Background(), c, Options{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "unknownInjectedValue") || len(s.Logs) != 0 ||
		len(s.MissingSecrets) != 1 || s.MissingSecrets[0] != render.AppSecretsSecret {
		t.Fatalf("missing-secret identity or fail-closed snapshot incorrect: %s", b)
	}
	expect(t, root(t, s), ConfigInvalid, "it needs the secret `PAYMENTS_API_KEY`, which this tenant has not configured", "secrets:")

	// Read denial proves nothing about existence; do not misdiagnose it as
	// a tenant forgetting to configure the referenced secret.
	c.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, render.AppSecretsSecret, nil)
	})
	s, err = Collect(context.Background(), c, Options{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.MissingSecrets) != 0 || strings.Contains(root(t, s).Summary, "this tenant has not configured") {
		t.Fatalf("read denial was reported as a missing tenant secret: %+v", s)
	}
}
