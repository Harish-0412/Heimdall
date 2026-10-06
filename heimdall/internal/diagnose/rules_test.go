package diagnose

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/render"
)

// Builders for hand-made snapshots. Captured fixtures (fixtures_test.go)
// cover the scenarios kind can reproduce; these cover the rest.

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

func pod(name, workload, component string, owner metav1.OwnerReference) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(t0),
		Labels:          map[string]string{labelName: workload, labelComponent: component},
		OwnerReferences: []metav1.OwnerReference{owner}}}
}

func rs(name string) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: "ReplicaSet", Name: name}
}
func sts(name string) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: "StatefulSet", Name: name}
}
func job(name string) metav1.OwnerReference { return metav1.OwnerReference{Kind: "Job", Name: name} }

func crashing(p corev1.Pod, exit int32, restarts int32) corev1.Pod {
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: restarts,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: exit}}}}
	return p
}

func notReady(p corev1.Pod) corev1.Pod {
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse})
	return p
}

func warning(kind, name, reason, message string) corev1.Event {
	return corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name + "." + reason}, Type: corev1.EventTypeWarning, Reason: reason,
		Message: message, InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name}, LastTimestamp: metav1.NewTime(t0)}
}

func failedJob(name, workload, component string, generation string) batchv1.Job {
	labels := map[string]string{labelName: workload, labelComponent: component, "heimdall.dev/generation": generation}
	return batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}}}
}

func jobPod(name, jobName, workload, component string, generation string, exit int32) corev1.Pod {
	p := pod(name, workload, component, job(jobName))
	p.Labels["heimdall.dev/generation"] = generation
	p.Labels[labelJobName] = jobName
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: exit}}}}
	return p
}

func root(t *testing.T, s *Snapshot) *Diagnosis {
	t.Helper()
	r := Diagnose(s)
	if r.RootCause() == nil {
		t.Fatal("no diagnosis")
	}
	return r.RootCause()
}

func expect(t *testing.T, d *Diagnosis, code Code, summary, suggestion string) {
	t.Helper()
	if d.Code != code || !strings.Contains(d.Summary, summary) || !strings.Contains(d.Suggestion, suggestion) {
		t.Errorf("got %s %q / %q\nwant %s %q / %q", d.Code, d.Summary, d.Suggestion, code, summary, suggestion)
	}
	if d.Title == "" || d.Severity == "" {
		t.Errorf("title/severity missing: %+v", d)
	}
}

func TestConfigProblems(t *testing.T) {
	s := &Snapshot{Failure: &Failure{Code: "agent.config_invalid", Message: "heimdall.yaml is invalid"}, ConfigProblems: []ConfigProblem{
		{Code: "field.unknown", Path: "services.api", Line: 6, Message: `unknown field "replicas" in service`},
		{Code: "policy.visibility", Path: "preview.visibility", Line: 30, Message: "public previews are not allowed"},
	}}
	r := Diagnose(s)
	if len(r.Diagnoses) != 2 {
		t.Fatalf("diagnoses: %+v", r.Diagnoses)
	}
	expect(t, &r.Diagnoses[0], ConfigInvalid, `line 6: services.api: field.unknown: unknown field "replicas"`, "heimdall validate")
	expect(t, &r.Diagnoses[1], PolicyDenied, "policy.visibility", "tenant's policy")
}

func TestFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		failure    Failure
		code       Code
		suggestion string
	}{
		{Failure{Code: "engine.stale_generation", Message: "generation 3 is older than 4"}, StaleGeneration, "newer push"},
		{Failure{Code: "engine.spec_invalid", Message: "render.image.missing: no image for \"api\""}, ConfigInvalid, "pinned by digest"},
		{Failure{Code: "engine.data_digest", Message: "the import does not match its approval"}, PolicyDenied, "approved SHA-256"},
		{Failure{Code: "engine.ownership", Message: "Deployment api is managed by kubectl"}, PolicyDenied, "belongs to someone else"},
		{Failure{Code: "engine.timeout", Step: "application/wave-1"}, Unclassified, "step timeout"},
		{Failure{Code: "engine.cluster", Message: "connection reset"}, Unclassified, "No rule explains"},
	} {
		d := root(t, &Snapshot{Failure: &tc.failure})
		if d.Code != tc.code || !strings.Contains(d.Suggestion, tc.suggestion) {
			t.Errorf("%s: got %s %q", tc.failure.Code, d.Code, d.Suggestion)
		}
	}
	if d := root(t, &Snapshot{Failure: &Failure{Code: "engine.stale_generation"}}); d.Severity != SeverityWarning {
		t.Errorf("superseded work is not an error: %+v", d)
	}
}

func TestNoCapacity(t *testing.T) {
	p := pod("api-1", "api", "service", rs("api-5d8f7"))
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/3 nodes are available: 2 Insufficient memory, 1 Insufficient cpu. preemption: 0/3 nodes are available."}}
	d := root(t, &Snapshot{Pods: []corev1.Pod{p}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}}})
	expect(t, d, NoCapacity, "api cannot be scheduled: no node has enough cpu or memory", "Lower `resources`")
	if d.Subject != "deployment/api" || d.Stage != "application" {
		t.Errorf("subject %s stage %s", d.Subject, d.Stage)
	}
}

// PostgreSQL crashing outranks the API that cannot reach it, which outranks
// the API's own crash and its missing endpoints.
func TestRankingFollowsTheCause(t *testing.T) {
	pg := notReady(crashing(pod("postgres-0", "postgres", "database", sts("postgres")), 1, 4))
	api := notReady(crashing(pod("api-1", "api", "service", rs("api-5d8f7")), 1, 3))
	s := &Snapshot{
		Pods:         []corev1.Pod{api, pg},
		Deployments:  []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		StatefulSets: []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "postgres"}}},
		Services: []corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "api", Labels: map[string]string{labelComponent: "service"}},
			Spec: corev1.ServiceSpec{Selector: map[string]string{labelName: "api"}}}},
		Logs: []Log{
			{Pod: "postgres-0", Container: "app", Previous: true, Text: "FATAL:  data directory \"/var/lib/postgresql/data\" has invalid permissions"},
			{Pod: "api-1", Container: "app", Previous: true, Text: "Error: connect ECONNREFUSED 10.96.4.2:5432\n    at TCPConnectWrap.afterConnect"},
		},
	}
	r := Diagnose(s)
	var got []string
	for _, d := range r.Diagnoses {
		got = append(got, string(d.Code)+" "+d.Subject)
	}
	want := []string{"CONTAINER_CRASH statefulset/postgres", "DB_UNREACHABLE deployment/api", "CONTAINER_CRASH deployment/api"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("ranking:\n got %v\nwant %v", got, want)
	}
	if r.Diagnoses[1].Summary != "api cannot reach PostgreSQL, which is not ready" {
		t.Errorf("DB summary: %q", r.Diagnoses[1].Summary)
	}
	if !strings.Contains(r.Diagnoses[0].Summary, "data directory") {
		t.Errorf("crash summary should quote the error line: %q", r.Diagnoses[0].Summary)
	}
}

func TestDatabaseCredentials(t *testing.T) {
	api := crashing(pod("api-1", "api", "service", rs("api-5d8f7")), 1, 2)
	s := &Snapshot{Pods: []corev1.Pod{api}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		Logs: []Log{{Pod: "api-1", Container: "app", Previous: true, Text: `error: password authentication failed for user "shopflow"`}}}
	expect(t, root(t, s), DBUnreachable, `cannot log in to PostgreSQL`, "DATABASE_URL")

	s.Logs[0].Text = "Error: getaddrinfo ENOTFOUND db.internal.example.com"
	expect(t, root(t, s), DBUnreachable, `connects to "db.internal.example.com", which does not exist`, "DATABASE_URL")
}

func TestSeedFailed(t *testing.T) {
	s := &Snapshot{Generation: 2,
		Jobs: []batchv1.Job{failedJob("heimdall-seed-g2", "heimdall-seed", "seed", "2")},
		Pods: []corev1.Pod{jobPod("heimdall-seed-g2-x", "heimdall-seed-g2", "heimdall-seed", "seed", "2", 3)},
		Logs: []Log{{Pod: "heimdall-seed-g2-x", Container: "main",
			Text: "psql:/seed/seed.sql:12: ERROR:  duplicate key value violates unique constraint \"products_sku_key\"\nDETAIL:  Key (sku)=([REDACTED]) already exists."}}}
	d := root(t, s)
	expect(t, d, SeedFailed, `Data import failed: duplicate key value violates unique constraint "products_sku_key" (SQLSTATE 23505)`, "Deduplicate")
	if d.SQL == nil || d.SQL.Constraint != "products_sku_key" || d.Stage != "baseline-db" {
		t.Errorf("sql %+v stage %s", d.SQL, d.Stage)
	}
}

func TestEndpointsWithoutPods(t *testing.T) {
	s := &Snapshot{Services: []corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "web", Labels: map[string]string{labelComponent: "service"}},
		Spec: corev1.ServiceSpec{Selector: map[string]string{labelName: "web"}}}},
		EndpointSlices: []discoveryv1.EndpointSlice{{ObjectMeta: metav1.ObjectMeta{Name: "web-x", Labels: map[string]string{"kubernetes.io/service-name": "web"}}}}}
	d := root(t, s)
	expect(t, d, NoEndpoints, "service web has no pods", "Nothing backs")
	if d.Severity != SeverityWarning {
		t.Errorf("severity %s", d.Severity)
	}
}

// A container the liveness probe keeps killing is a health-check problem,
// not a crash.
func TestLivenessKillsAreHealthChecks(t *testing.T) {
	p := notReady(crashing(pod("api-1", "api", "service", rs("api-5d8f7")), 137, 3))
	p.Spec.Containers = []corev1.Container{{Name: "app", LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/live", Port: intstr.FromInt32(8080)}}}}}
	s := &Snapshot{Pods: []corev1.Pod{p}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		Events: []corev1.Event{warning("Pod", "api-1", "Unhealthy", "Liveness probe failed: Get \"http://10.244.0.7:8080/live\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)")}}
	r := Diagnose(s)
	if len(r.Diagnoses) != 1 {
		t.Fatalf("diagnoses: %+v", r.Diagnoses)
	}
	expect(t, &r.Diagnoses[0], HealthcheckFailed, "api fails its liveness check: GET /live on port 8080 does not answer in time", "restart it")
}

func TestQuotaFromDeploymentCondition(t *testing.T) {
	d := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Labels: map[string]string{labelComponent: "service"}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentReplicaFailure,
			Status: corev1.ConditionTrue, Reason: "FailedCreate",
			Message: `pods "web-7c9-x" is forbidden: exceeded quota: heimdall-quota, requested: limits.memory=256Mi, used: limits.memory=2Gi, limited: limits.memory=2Gi`}}}}
	q := corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-quota"}, Status: corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourceLimitsMemory: resource.MustParse("2Gi"), corev1.ResourcePods: resource.MustParse("20")},
		Used: corev1.ResourceList{corev1.ResourceLimitsMemory: resource.MustParse("2Gi"), corev1.ResourcePods: resource.MustParse("9")}}}
	got := root(t, &Snapshot{Deployments: []appsv1.Deployment{d}, Quotas: []corev1.ResourceQuota{q}})
	expect(t, got, QuotaExceeded, "(requested limits.memory=256Mi; used limits.memory=2Gi of limits.memory=2Gi)", "quota is sized")
	if got.Stage != "application" || len(got.Evidence) != 2 || got.Evidence[1] != "quota heimdall-quota: limits.memory used 2Gi of 2Gi" {
		t.Errorf("stage %s evidence %q", got.Stage, got.Evidence)
	}
}

func TestNothingWrong(t *testing.T) {
	r := Diagnose(&Snapshot{Namespace: "heimdall-pr1-x"})
	if r.RootCause() != nil || r.Diagnoses == nil {
		t.Errorf("report: %+v", r)
	}
}

func TestEvidenceIsBounded(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, strings.Repeat("x", 300)+string(rune('a'+i%26)))
	}
	got := tidy(append(lines, lines[0], "", "  "))
	if len(got) != maxEvidenceLines {
		t.Errorf("%d lines", len(got))
	}
	for _, l := range got {
		if len([]rune(l)) > maxEvidenceWidth {
			t.Errorf("line of %d runes", len([]rune(l)))
		}
	}
}

func TestCodesAreCatalogued(t *testing.T) {
	seen := map[Code]bool{}
	for _, c := range Codes() {
		if c.Title() == "" || seen[c] || strings.ToUpper(string(c)) != string(c) {
			t.Errorf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) != len(catalog) {
		t.Errorf("%d codes listed, %d catalogued", len(seen), len(catalog))
	}
}

func route(name string, conditions ...metav1.Condition) gatewayv1.HTTPRoute {
	ns := gatewayv1.Namespace("heimdall-gateway")
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{labelComponent: "route"},
			Annotations: map[string]string{"heimdall.dev/url": "http://pr7-demo-abcd.wrong.test"}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef: gatewayv1.ParentReference{Namespace: &ns, Name: "heimdall"}, Conditions: conditions}}}}}
}

func TestRouteRejectedByTheGateway(t *testing.T) {
	r := route("web", metav1.Condition{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NoMatchingListenerHostname",
		Message: "Listener hostname does not match the HTTPRoute hostnames"})
	d := root(t, &Snapshot{Routes: []gatewayv1.HTTPRoute{r}})
	expect(t, d, NoEndpoints, "http://pr7-demo-abcd.wrong.test is not served: Gateway heimdall-gateway/heimdall rejected route web (NoMatchingListenerHostname)", "baseDomain")
	if d.Subject != "httproute/web" || d.Stage != "application" || d.Severity != SeverityError {
		t.Errorf("%+v", d)
	}
	// Even with the Service's pods crashing, the rejected route is reported:
	// it is a problem of its own, not a symptom.
	web := crashing(pod("web-1", "web", "service", rs("web-5d8f7")), 1, 3)
	r2 := Diagnose(&Snapshot{Routes: []gatewayv1.HTTPRoute{r}, Pods: []corev1.Pod{web},
		Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}})
	if len(r2.Diagnoses) != 2 || r2.Diagnoses[0].Code != ContainerCrash || r2.Diagnoses[1].Code != NoEndpoints {
		t.Errorf("diagnoses: %+v", r2.Diagnoses)
	}
	// No status: no Gateway controller has looked at it; nothing to say.
	if r3 := Diagnose(&Snapshot{Routes: []gatewayv1.HTTPRoute{route("web")}}); len(r3.Diagnoses) != 0 {
		t.Errorf("a route without status was diagnosed: %+v", r3.Diagnoses)
	}
	accepted := route("web", metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"},
		metav1.Condition{Type: "ResolvedRefs", Status: metav1.ConditionFalse, Reason: "BackendNotFound", Message: "Service web not found"})
	expect(t, root(t, &Snapshot{Routes: []gatewayv1.HTTPRoute{accepted}}), NoEndpoints, "points at a backend", "heimdall up")
}

func TestMissingTenantSecret(t *testing.T) {
	p := pod("api-1", "api", "service", rs("api-5d8f7"))
	p.Spec.Containers = []corev1.Container{{Name: "api", Env: []corev1.EnvVar{
		{Name: "STRIPE_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: render.AppSecretsSecret}, Key: "STRIPE_KEY"}}},
		{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: render.CredentialsSecret}, Key: "DATABASE_URL"}}},
	}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "CreateContainerConfigError", Message: `secret "heimdall-app-secrets" not found`}}}}
	s := &Snapshot{Pods: []corev1.Pod{p}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}}}
	expect(t, root(t, s), ConfigInvalid, "api cannot start: it needs the secret `STRIPE_KEY`, which this tenant has not configured", "tenant's secrets")

	s.Pods[0].Status.ContainerStatuses[0].State.Waiting.Message = "couldn't find key PAYMENTS_TOKEN in Secret heimdall-pr7/heimdall-app-secrets"
	expect(t, root(t, s), ConfigInvalid, "it needs the secret `PAYMENTS_TOKEN`", "remove it from `secrets:`")
}

func TestPostgresNotRunning(t *testing.T) {
	api := notReady(pod("api-1", "api", "service", rs("api-5d8f7")))
	api.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	zero := int32(0)
	s := &Snapshot{Pods: []corev1.Pod{api}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		StatefulSets: []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "postgres", Labels: map[string]string{labelComponent: "database"}},
			Spec: appsv1.StatefulSetSpec{Replicas: &zero}}},
		Events: []corev1.Event{warning("Pod", "api-1", "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 500")},
		Logs: []Log{{Pod: "api-1", Container: "api",
			Text: `{"time":"2026-10-02T09:00:00Z","event":"request.failed","path":"/health","message":"connect ECONNREFUSED 10.96.4.2:5432"}`}}}
	r := Diagnose(s)
	if len(r.Diagnoses) != 1 {
		t.Fatalf("the failing health check is a symptom: %+v", r.Diagnoses)
	}
	expect(t, &r.Diagnoses[0], DBUnreachable, "PostgreSQL is not running (statefulset/postgres is scaled to zero): api cannot reach it", "PostgreSQL is down")
	if d := r.Diagnoses[0]; d.Subject != "statefulset/postgres" || len(d.Evidence) != 2 || !strings.HasPrefix(d.Evidence[1], "api: ") {
		t.Errorf("subject %s evidence %q", d.Subject, d.Evidence)
	}
}

func TestStepTimeoutSaysWhatWasPending(t *testing.T) {
	j := failedJob("heimdall-migrate-g1", "heimdall-migrate", "migration", "1")
	j.Labels["heimdall.dev/stage"], j.Spec.Template.Labels["heimdall.dev/stage"] = "baseline-db", "baseline-db"
	j.Status = batchv1.JobStatus{Active: 1}
	s := &Snapshot{Generation: 1, Failure: &Failure{Code: "engine.timeout", Step: "baseline-db/migrate"}, Jobs: []batchv1.Job{j}}
	d := root(t, s)
	expect(t, d, Unclassified, "step baseline-db/migrate did not finish within the step timeout: job/heimdall-migrate-g1 was still running", "lock")
	if d.Subject != "job/heimdall-migrate-g1" {
		t.Errorf("subject %s", d.Subject)
	}
}

func TestFailureOnlySnapshot(t *testing.T) {
	s := ForFailure("heimdall-pr7-demo-abcd", 2, &Failure{Code: "engine.data_digest",
		Message: "import bytes do not match the approval for postgres://app:hunter2hunter2@db/app"}, errors.New("namespaces \"heimdall-pr7-demo-abcd\" not found"))
	if strings.Contains(s.Failure.Message, "hunter2hunter2") || len(s.Notes) != 1 {
		t.Errorf("%+v", s)
	}
	expect(t, root(t, s), PolicyDenied, "do not match the approval", "approved SHA-256")
}

func TestStaleGenerationNamesBoth(t *testing.T) {
	s := &Snapshot{Generation: 1, AcceptedGeneration: 2, Failure: &Failure{Code: "engine.stale_generation",
		Message: "generation is older than the accepted specification"}}
	expect(t, root(t, s), StaleGeneration, "generation 1 is older than generation 2, which this preview already accepted", "newer push")
}

func TestRouteNamesTheListener(t *testing.T) {
	r := route("web", metav1.Condition{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAllowedByListeners"})
	section := gatewayv1.SectionName("http")
	r.Status.Parents[0].ParentRef.SectionName = &section
	expect(t, root(t, &Snapshot{Routes: []gatewayv1.HTTPRoute{r}}), NoEndpoints,
		"Gateway heimdall-gateway/heimdall (listener http) rejected route web (NotAllowedByListeners)", "heimdall.dev/preview=true")
}

// The migration Job runs with its service's environment: a secret missing
// there stops the Job's pod from starting, and the Job never fails.
func TestJobPodThatCannotStart(t *testing.T) {
	p := jobPod("heimdall-migrate-g1-x", "heimdall-migrate-g1", "heimdall-migrate", "migration", "1", 0)
	p.Spec.Containers = []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "PAYMENTS_API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: render.AppSecretsSecret}, Key: "PAYMENTS_API_KEY"}}}}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "CreateContainerConfigError", Message: `secret "heimdall-app-secrets" not found`}}}}
	j := failedJob("heimdall-migrate-g1", "heimdall-migrate", "migration", "1")
	j.Status = batchv1.JobStatus{Active: 1}
	s := &Snapshot{Generation: 1, Jobs: []batchv1.Job{j}, Pods: []corev1.Pod{p},
		Failure: &Failure{Code: "engine.workload_failed", Step: "baseline-db/migrate"}}
	d := root(t, s)
	expect(t, d, ConfigInvalid, "The migration Job cannot start: it needs the secret `PAYMENTS_API_KEY`", "lists `PAYMENTS_API_KEY` under `secrets:`, but")
	if d.Subject != "job/heimdall-migrate-g1" || d.Stage != "baseline-db" {
		t.Errorf("subject %s stage %s", d.Subject, d.Stage)
	}
}

func TestLostConnectionCountsAsUnreachable(t *testing.T) {
	api := crashing(pod("api-1", "api", "service", rs("api-5d8f7")), 1, 1)
	zero := int32(0)
	s := &Snapshot{Pods: []corev1.Pod{api}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		StatefulSets: []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "postgres", Labels: map[string]string{labelComponent: "database"}},
			Spec: appsv1.StatefulSetSpec{Replicas: &zero}}},
		Logs: []Log{{Pod: "api-1", Container: "app", Previous: true, Text: "Error: Connection terminated unexpectedly\n[... 200 lines omitted]\nNode.js v24.21.0"}}}
	expect(t, root(t, s), DBUnreachable, "PostgreSQL is not running (statefulset/postgres is scaled to zero): api cannot reach it", "PostgreSQL is down")

	// PostgreSQL running, the connection dropped anyway (a restart).
	s.StatefulSets = nil
	expect(t, root(t, s), DBUnreachable, "api lost its connection to PostgreSQL: Connection terminated unexpectedly", "reconnect")
}

// When the previous instance's log is gone, the termination message (the
// tail of the same output) explains the crash.
func TestCrashFromTerminationMessage(t *testing.T) {
	p := crashing(pod("notifications-1", "notifications", "worker", rs("notifications-5d8f7")), 1, 3)
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.Message = "node:internal/modules/cjs/loader:1568\n  throw err;\n  ^\n\n" +
		"Error: Cannot find module '/app/src/notifier.js'\n    at Module._resolveFilename (node:internal/modules/cjs/loader:1565:15)\n  code: 'MODULE_NOT_FOUND',\n}"
	s := &Snapshot{Pods: []corev1.Pod{p}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "notifications"}}},
		Logs: []Log{{Pod: "notifications-1", Container: "app", Previous: true, Text: "unable to retrieve container logs for containerd://caf87c4e"}}}
	d := root(t, s)
	expect(t, d, ContainerCrash, "notifications exits with code 1, 3 restarts: Error: Cannot find module '/app/src/notifier.js'", "heimdall logs")
	if len(d.Evidence) < 2 || d.Evidence[1] != "Error: Cannot find module '/app/src/notifier.js'" {
		t.Errorf("evidence %q", d.Evidence)
	}
}

func TestHistoricalObjectsCannotChangeTheReport(t *testing.T) {
	s := &Snapshot{Generation: 2, Failure: &Failure{Code: "engine.timeout", Step: "application/wave-1"}}
	want := Diagnose(s)
	old := map[string]string{render.LabelGeneration: "1", labelComponent: "service"}
	s.Deployments = []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "old-api", Labels: old},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: "ReplicaFailure", Status: corev1.ConditionTrue,
			Message: "exceeded quota: old"}}}}}
	s.StatefulSets = []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "old-postgres", Labels: map[string]string{
		render.LabelGeneration: "1", labelComponent: "database"}}}}
	s.Services = []corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "old-service", Labels: old},
		Spec: corev1.ServiceSpec{Selector: map[string]string{labelName: "old-api"}}}}
	s.Events = []corev1.Event{warning("Deployment", "old-api", "FailedCreate", "exceeded quota: old")}
	if got := Diagnose(s); !reflect.DeepEqual(got, want) {
		t.Errorf("old-generation objects changed the report: got %+v; want %+v", got, want)
	}
	s.Deployments[0].Labels[render.LabelGeneration] = "invalid"
	if got := Diagnose(s); !reflect.DeepEqual(got, want) {
		t.Errorf("malformed generation was treated as current: %+v", got)
	}
}

func TestEventsFromReplacedObjectsAreIgnored(t *testing.T) {
	p := notReady(pod("api-1", "api", "service", rs("api-current")))
	p.UID = "current-pod"
	p.OwnerReferences[0].UID = "current-rs"
	s := &Snapshot{Pods: []corev1.Pod{p}, Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api"}}},
		Events: []corev1.Event{warning("Pod", "api-1", "Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500"),
			warning("ReplicaSet", "api-previous", "FailedCreate", "exceeded quota: old")}}
	s.Events[0].InvolvedObject.UID = "replaced-pod"
	if got := Diagnose(s); len(got.Diagnoses) != 0 {
		t.Fatalf("historical events changed the report: %+v", got.Diagnoses)
	}
	s.Events[0].InvolvedObject.UID = p.UID
	expect(t, root(t, s), HealthcheckFailed, "liveness", "restart")
}

func TestRecoveredCrashAndOOMAreIgnored(t *testing.T) {
	for _, reason := range []string{"Error", "OOMKilled"} {
		p := crashing(pod("api-1", "api", "service", rs("api-current")), 137, 3)
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		p.Status.ContainerStatuses[0].Ready = true
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.Reason = reason
		if got := Diagnose(&Snapshot{Pods: []corev1.Pod{p}}); len(got.Diagnoses) != 0 {
			t.Errorf("recovered %s is reported as failing: %+v", reason, got.Diagnoses)
		}
	}
}

func TestOldGatewayVerdictsAreIgnored(t *testing.T) {
	r := route("web", metav1.Condition{Type: "Accepted", Status: metav1.ConditionFalse,
		Reason: "NoMatchingListenerHostname", ObservedGeneration: 1})
	r.Generation = 2
	if got := Diagnose(&Snapshot{Routes: []gatewayv1.HTTPRoute{r}}); len(got.Diagnoses) != 0 {
		t.Errorf("old route verdict reported: %+v", got.Diagnoses)
	}
	r.Status.Parents[0].Conditions[0].ObservedGeneration = 2
	expect(t, root(t, &Snapshot{Routes: []gatewayv1.HTTPRoute{r}}), NoEndpoints, "not served", "baseDomain")
	r.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "new-gateway"}}
	if got := Diagnose(&Snapshot{Routes: []gatewayv1.HTTPRoute{r}}); len(got.Diagnoses) != 0 {
		t.Errorf("removed Gateway verdict reported: %+v", got.Diagnoses)
	}
}

func TestWarningsDoNotHideAnUnexplainedFailure(t *testing.T) {
	s := &Snapshot{Failure: &Failure{Code: "engine.cluster", Message: "connection reset"},
		Services: []corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{labelName: "web"}}}}}
	r := Diagnose(s)
	for i := range r.Diagnoses {
		if r.Diagnoses[i].Code == Unclassified {
			expect(t, &r.Diagnoses[i], Unclassified, "engine.cluster: connection reset", "No rule explains")
			return
		}
	}
	t.Errorf("warnings hid the recorded failure: %+v", r.Diagnoses)
}

func TestLostJournalOwnershipIsTheOperationRootCause(t *testing.T) {
	healthy := pod("rabbitmq-1", "rabbitmq", "broker", rs("rabbitmq-current"))
	healthy.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	unready := notReady(pod("rabbitmq-1", "rabbitmq", "broker", rs("rabbitmq-current")))
	unready.Spec.Containers = []corev1.Container{{Name: "queue", ReadinessProbe: &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"rabbitmq-diagnostics", "ping"}}}}}}
	image := pod("api-1", "api", "service", rs("api-current"))
	image.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "image not found"}}}}
	for _, tc := range []struct {
		name      string
		pods      []corev1.Pod
		events    []corev1.Event
		secondary Code
	}{
		{"healthy workload", []corev1.Pod{healthy}, nil, ""},
		{"dependency readiness", []corev1.Pod{unready}, []corev1.Event{warning("Pod", "rabbitmq-1", "Unhealthy", "Readiness probe failed: connection refused")}, HealthcheckFailed},
		{"application image failure", []corev1.Pod{image}, nil, ImagePullFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{Pods: tc.pods, Events: tc.events, Failure: &Failure{Code: "engine.lock_lost",
				Message: "operation journal ownership was lost", Step: "dependencies/start"},
				Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "rabbitmq"}}, {ObjectMeta: metav1.ObjectMeta{Name: "api"}}}}
			r := Diagnose(s)
			d := r.RootCause()
			if d == nil {
				t.Fatal("no diagnosis")
			}
			expect(t, d, Unclassified, "engine.lock_lost: operation journal ownership was lost", "Kubernetes API connectivity")
			if d.Subject != "dependencies/start" || d.Stage != "dependencies" || !strings.Contains(d.Suggestion, "lease expires") {
				t.Errorf("lost-ownership diagnosis: %+v", d)
			}
			if tc.secondary != "" {
				if len(r.Diagnoses) != 2 || r.Diagnoses[1].Code != tc.secondary {
					t.Errorf("independent finding was lost or outranked ownership: %+v", r.Diagnoses)
				}
				// Other operation failures retain the normal workload ranking.
				s.Failure.Code = "engine.workload_failed"
				if got := Diagnose(s).RootCause(); got == nil || got.Code != tc.secondary {
					t.Errorf("non-ownership failure ranking changed: %+v", got)
				}
			}
		})
	}
}

func TestFailedJobInitContainerKeepsItsError(t *testing.T) {
	p := jobPod("migrate-x", "heimdall-migrate-g2", "heimdall-migrate", "migration", "2", 0)
	p.Status.ContainerStatuses = nil
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: `ERROR: relation "orders" does not exist`}}}}
	s := &Snapshot{Generation: 2, Pods: []corev1.Pod{p}, Jobs: []batchv1.Job{failedJob("heimdall-migrate-g2", "heimdall-migrate", "migration", "2")}}
	expect(t, root(t, s), MigrationFailed, `relation "orders" does not exist (SQLSTATE 42P01)`, "must run first")
}

func TestSupersededReplicaSetPodsAreIgnored(t *testing.T) {
	labels := map[string]string{labelName: "api", labelComponent: "service"}
	current := pod("api-new-1", "api", "service", rs("api-new"))
	current.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry/api@sha256:current"}}
	old := crashing(pod("api-old-1", "api", "service", rs("api-old")), 1, 4)
	old.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry/api@sha256:previous"}}
	s := &Snapshot{Generation: 2, Pods: []corev1.Pod{current, old}, Deployments: []appsv1.Deployment{{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Labels: map[string]string{render.LabelGeneration: "2"}},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: current.Spec}},
	}}}
	s.Sanitize(nil)
	if got := Diagnose(s); len(got.Diagnoses) != 0 {
		t.Errorf("old rollout crash was attributed to current generation: %+v", got.Diagnoses)
	}
	s.Pods[0] = crashing(s.Pods[0], 1, 3)
	expect(t, root(t, s), ContainerCrash, "api exits with code 1", "heimdall logs")
}

func TestClearedTemplateFieldsExcludeAnOldCrashingPod(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  func(*corev1.Container)
	}{
		{"command removed", func(c *corev1.Container) { c.Command = []string{"old-command"} }},
		{"literal environment cleared", func(c *corev1.Container) { c.Env[0].Value = "old-mode" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := pod("api-new-1", "api", "service", rs("api-new"))
			current.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry/api@sha256:same",
				Env: []corev1.EnvVar{{Name: "MODE", Value: ""}}}}
			current.Spec.NodeName = "node-a"
			current.Spec.DNSPolicy = corev1.DNSClusterFirst
			current.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
			old := crashing(*current.DeepCopy(), 1, 4)
			old.Name = "api-old-1"
			old.OwnerReferences = []metav1.OwnerReference{rs("api-old")}
			tc.old(&old.Spec.Containers[0])
			template := *current.Spec.DeepCopy()
			template.NodeName = ""
			template.DNSPolicy = ""
			template.Containers[0].ImagePullPolicy = ""
			s := &Snapshot{Pods: []corev1.Pod{current, old}, Deployments: []appsv1.Deployment{{
				ObjectMeta: metav1.ObjectMeta{Name: "api"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: current.Labels}, Spec: template}},
			}}}
			s.Sanitize(nil)
			if got := Diagnose(s); len(got.Diagnoses) != 0 {
				t.Errorf("cleared field attributed an obsolete crash to the current rollout: %+v", got.Diagnoses)
			}
			if len(s.Pods) != 1 || s.Pods[0].Name != current.Name {
				t.Fatalf("sanitization retained an obsolete pod or excluded the defaulted current pod: %+v", s.Pods)
			}
			s.Pods[0] = crashing(s.Pods[0], 1, 3)
			expect(t, root(t, s), ContainerCrash, "api exits with code 1", "heimdall logs")
		})
	}
}

func TestRecoveredLivenessWarningIsIgnored(t *testing.T) {
	p := pod("api-1", "api", "service", rs("api-current"))
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(t0.Add(time.Minute))}}}}
	s := &Snapshot{Pods: []corev1.Pod{p}, Events: []corev1.Event{
		warning("Pod", p.Name, "Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500")}}
	if got := Diagnose(s); len(got.Diagnoses) != 0 {
		t.Errorf("old liveness warning was attributed to recovered container: %+v", got.Diagnoses)
	}
	s.Events[0].LastTimestamp = metav1.NewTime(t0.Add(2 * time.Minute))
	expect(t, root(t, s), HealthcheckFailed, "liveness", "restart")
}

func TestCondensedCrashKeepsItsPreservedFirstError(t *testing.T) {
	p := crashing(pod("api-1", "api", "service", rs("api-current")), 1, 3)
	logs := "Error: missing required configuration\n" + strings.Repeat("  dumpedField: null,\n", 200) + "runtime finished"
	s := &Snapshot{Pods: []corev1.Pod{p}, Logs: []Log{{Pod: p.Name, Container: "app", Previous: true,
		Text: condense(logs, 60, 16<<10)}}}
	d := root(t, s)
	expect(t, d, ContainerCrash, "Error: missing required configuration", "heimdall logs")
	if len(d.Evidence) < 2 || d.Evidence[1] != "Error: missing required configuration" {
		t.Errorf("condensed first error fell out of the evidence: %+v", d.Evidence)
	}
}

func TestCrashIgnoresPseudoErrorsInObjectDumps(t *testing.T) {
	logs := "  error: [Function: bound _handleErrorEvent],\n" +
		"  error: undefined,\n  error: null,\n  error: [Object],\n" +
		"Error: missing required configuration\n  at run (app.js:1:1)"
	if got := firstError(logs); got != "Error: missing required configuration" {
		t.Errorf("object-dump property was selected as the crash: %q", got)
	}
	if got := crashEvidence(logs); len(got) != 1 || got[0] != "Error: missing required configuration" {
		t.Errorf("pseudo-error or stack frame leaked into evidence: %q", got)
	}
	if got := firstError("  error: [Function: bound _handleErrorEvent],\n  error: undefined,"); got != "" {
		t.Errorf("dump without a real error generated a summary: %q", got)
	}
}
