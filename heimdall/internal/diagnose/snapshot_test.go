package diagnose

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

func TestSanitizeAllRetainedPayloads(t *testing.T) {
	secret := "snapshot-secret-value"
	metadata := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Labels: map[string]string{labelName: "api", labelComponent: "service"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}},
			Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "entire-spec", "custom": secret}}
	}
	container := func() corev1.Container {
		return corev1.Container{Name: "app", Env: []corev1.EnvVar{{Name: "PRIVATE", Value: secret}},
			Command: []string{"sh", "-c", "echo " + secret},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health", Port: intstr.FromInt32(8080), HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Key", Value: secret}}}}},
			Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{secret}}}}}
	}
	template := func() corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{ObjectMeta: metadata(), Spec: corev1.PodSpec{
			Containers: []corev1.Container{container()}, InitContainers: []corev1.Container{container()},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name: "debug", Env: []corev1.EnvVar{{Name: "DEBUG_KEY", Value: secret}}}}},
			Volumes: []corev1.Volume{{Name: "driver", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
				Driver: "example", VolumeAttributes: map[string]string{"credential": secret}}}}}}}
	}
	podTemplate := template()
	s := &Snapshot{Generation: 3, Failure: &Failure{Code: "engine.cluster", Message: secret},
		ConfigProblems: []ConfigProblem{{Code: "field.invalid", Message: secret}}, Notes: []string{secret},
		Pods: []corev1.Pod{{ObjectMeta: metadata(), Spec: podTemplate.Spec,
			Status: corev1.PodStatus{Message: secret, EphemeralContainerStatuses: []corev1.ContainerStatus{{Name: "debug", Image: secret}}}}},
		Jobs:         []batchv1.Job{{ObjectMeta: metadata(), Spec: batchv1.JobSpec{Template: template()}}},
		Deployments:  []appsv1.Deployment{{ObjectMeta: metadata(), Spec: appsv1.DeploymentSpec{Template: template()}}},
		StatefulSets: []appsv1.StatefulSet{{ObjectMeta: metadata(), Spec: appsv1.StatefulSetSpec{Template: template()}}},
		Services:     []corev1.Service{{ObjectMeta: metadata()}}, Events: []corev1.Event{{ObjectMeta: metadata(), Message: secret}},
		EndpointSlices: []discoveryv1.EndpointSlice{{ObjectMeta: metadata()}}, Quotas: []corev1.ResourceQuota{{ObjectMeta: metadata()}},
		Routes: []gatewayv1.HTTPRoute{{ObjectMeta: metadata(), Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			Filters: []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Key", Value: secret}}}}},
		}}}}}, Logs: []Log{{Text: secret}}}
	s.Sanitize(redact.New(secret))
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "managedFields", "last-applied-configuration", "entire-spec", "DEBUG_KEY"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("snapshot retained %q: %s", forbidden, b)
		}
	}
	for _, keep := range []string{`"name":"PRIVATE"`, `"path":"/health"`, `"generation":3`, redact.Marker} {
		if !strings.Contains(string(b), keep) {
			t.Errorf("snapshot lost diagnostic field %q", keep)
		}
	}
	s.Sanitize(redact.New(secret))
	again, _ := json.Marshal(s)
	if !reflect.DeepEqual(b, again) {
		t.Error("sanitization is not idempotent")
	}
}

func TestOmitSensitiveTextWhenSecretValuesAreUnavailable(t *testing.T) {
	secret := "unknown-credential-value"
	p := crashing(pod("api-1", "api", "service", rs("api-current")), 1, 3)
	p.Annotations = map[string]string{"custom": secret}
	p.Labels["custom"] = secret
	p.Spec.Containers = []corev1.Container{{Name: "app", Args: []string{secret}, Image: secret}}
	p.Status.Message = secret
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.Message = secret
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: secret}}
	s := &Snapshot{Pods: []corev1.Pod{p}, Failure: &Failure{Code: "engine.workload_failed", Message: secret},
		ConfigProblems: []ConfigProblem{{Code: "field.invalid", Message: secret}},
		Events:         []corev1.Event{warning("Pod", p.Name, "BackOff", secret)}, Logs: []Log{{Text: secret}}, Notes: []string{secret}}
	s.Sanitize(redact.New())
	s.OmitSensitiveText()
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), secret) {
		t.Fatalf("unredacted text survived without readable secrets: %s", b)
	}
	if len(s.Logs) != 0 || !strings.Contains(s.Notes[0], "logs omitted") {
		t.Errorf("omission evidence missing: %+v", s)
	}
	if got := Diagnose(s); got.RootCause() == nil || got.RootCause().Code != ConfigInvalid {
		t.Errorf("structured config failure was lost: %+v", got)
	}
}

func TestShortInjectedSecretsPreserveStructuralLabels(t *testing.T) {
	p := crashing(pod("api-1", "api", "service", rs("api-current")), 1, 3)
	p.Labels[render.LabelGeneration] = "1"
	p.Labels[render.LabelStage] = "application"
	p.Labels["app-controlled"] = "extra-secret-value"
	old := crashing(pod("api-old", "api", "service", rs("api-previous")), 1, 10)
	old.Labels[render.LabelGeneration] = "0"
	s := &Snapshot{Generation: 1, Pods: []corev1.Pod{p, old},
		Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api", Labels: map[string]string{render.LabelGeneration: "1"}}}}}
	s.Sanitize(redact.New("1", "service", "application", "api", "extra-secret-value"))
	r := Diagnose(s)
	if len(r.Diagnoses) != 1 {
		t.Fatalf("short secrets changed current-generation findings: %+v", r.Diagnoses)
	}
	expect(t, r.RootCause(), ContainerCrash, "api exits with code 1, 3 restarts", "heimdall logs")
	if r.RootCause().Subject != "deployment/api" || r.RootCause().Stage != "application" {
		t.Errorf("public identity or structural stage was redacted: %+v", r.RootCause())
	}
	if _, retained := s.Pods[0].Labels["app-controlled"]; retained {
		t.Error("arbitrary app metadata label was retained")
	}
}

func TestSanitizeFiltersObsoleteRolloutsBeforeRemovingSpecDifferences(t *testing.T) {
	const currentValue, oldValue = "rollout-current-secret", "rollout-previous-secret"
	for _, tc := range []struct {
		name   string
		change func(*corev1.PodSpec, string)
	}{
		{"literal env", func(p *corev1.PodSpec, value string) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "SETTING", Value: value}}
		}},
		{"probe header", func(p *corev1.PodSpec, value string) {
			p.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080),
					HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Key", Value: value}}}}}
		}},
		{"envFrom", func(p *corev1.PodSpec, value string) {
			p.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: value}}}}
		}},
		{"volume attributes", func(p *corev1.PodSpec, value string) {
			p.Volumes = []corev1.Volume{{Name: "driver", VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{Driver: "example.driver", VolumeAttributes: map[string]string{"credential": value}}}}}
		}},
		{"lifecycle", func(p *corev1.PodSpec, value string) {
			p.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{Command: []string{value}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := pod("api-current-1", "api", "service", rs("api-current"))
			current.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry/api@sha256:current"}}
			tc.change(&current.Spec, currentValue)
			current.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			old := crashing(pod("api-previous-1", "api", "service", rs("api-previous")), 1, 3)
			old.Spec = *current.Spec.DeepCopy()
			tc.change(&old.Spec, oldValue)
			s := &Snapshot{Generation: 2, Pods: []corev1.Pod{old, current},
				Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api",
					Labels: map[string]string{render.LabelGeneration: "2"}}, Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: current.Labels},
						Spec: *current.Spec.DeepCopy()}}}},
				Events: []corev1.Event{warning("Pod", old.Name, "BackOff", "Back-off restarting failed container "+oldValue)},
				Logs:   []Log{{Pod: old.Name, Container: "app", Previous: true, Text: "Error: " + oldValue}}}
			if s.matchesCurrentTemplate(&old) || !s.matchesCurrentTemplate(&current) {
				t.Fatal("scenario does not distinguish the raw obsolete and current templates")
			}
			r := redact.New(currentValue, oldValue)
			s.Sanitize(r)
			if len(s.Pods) != 1 || s.Pods[0].Name != current.Name {
				t.Fatalf("sanitization retained obsolete pod or discarded current pod: %+v", s.Pods)
			}
			if got := Diagnose(s); len(got.Diagnoses) != 0 {
				t.Errorf("obsolete rollout polluted current diagnosis: %+v", got.Diagnoses)
			}
			before, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(before), currentValue) || strings.Contains(string(before), oldValue) {
				t.Fatalf("known secret survived sanitization: %s", before)
			}
			s.Sanitize(r)
			after, _ := json.Marshal(s)
			if !reflect.DeepEqual(before, after) {
				t.Error("repeat sanitization changed the filtered snapshot")
			}
		})
	}
}
