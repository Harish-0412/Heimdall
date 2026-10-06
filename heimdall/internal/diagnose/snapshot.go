package diagnose

import (
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// SnapshotVersion is the snapshot format; captured fixtures record it.
const SnapshotVersion = 1

// Snapshot is everything the rules look at, for one preview namespace at one
// moment. It holds no secret values: Collect strips environment values and
// redacts every message and log line before they are stored.
type Snapshot struct {
	Version    int       `json:"version"`
	CapturedAt time.Time `json:"capturedAt"`
	Namespace  string    `json:"namespace"`
	// Generation the environment is being deployed at. Objects labelled with
	// another generation are history and ignored.
	Generation int64 `json:"generation"`
	// AcceptedGeneration is the generation the engine's journal last
	// accepted for this preview (0 when unknown).
	AcceptedGeneration int64 `json:"acceptedGeneration,omitempty"`
	// Failure is what the engine or agent reported, when an operation failed.
	Failure *Failure `json:"failure,omitempty"`
	// ConfigProblems are the configuration's error diagnostics, when the
	// configuration itself was rejected.
	ConfigProblems []ConfigProblem `json:"configProblems,omitempty"`
	// MissingSecrets records Secret identities confirmed absent by the API.
	// It preserves actionable configuration errors when untrusted messages
	// must be omitted because secret values could not be read.
	MissingSecrets []string `json:"missingSecrets,omitempty"`

	Pods           []corev1.Pod                `json:"pods,omitempty"`
	Events         []corev1.Event              `json:"events,omitempty"`
	Jobs           []batchv1.Job               `json:"jobs,omitempty"`
	Deployments    []appsv1.Deployment         `json:"deployments,omitempty"`
	StatefulSets   []appsv1.StatefulSet        `json:"statefulSets,omitempty"`
	Services       []corev1.Service            `json:"services,omitempty"`
	EndpointSlices []discoveryv1.EndpointSlice `json:"endpointSlices,omitempty"`
	Quotas         []corev1.ResourceQuota      `json:"quotas,omitempty"`
	// Routes are the preview's HTTPRoutes, with the Gateway's verdict on
	// each (status.parents): how the preview URL reaches its services.
	Routes []gatewayv1.HTTPRoute `json:"routes,omitempty"`
	Logs   []Log                 `json:"logs,omitempty"`
	// Notes record what could not be collected, and why.
	Notes []string `json:"notes,omitempty"`
}

// Failure is the engine's or agent's own report of a failed operation.
type Failure struct {
	Code    string `json:"code"` // e.g. engine.job_failed, agent.config_invalid
	Message string `json:"message,omitempty"`
	Step    string `json:"step,omitempty"` // e.g. baseline-db/migrate
}

// ConfigProblem is one error diagnostic of heimdall.yaml.
type ConfigProblem struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// Log is the redacted, size-capped tail of one container's output.
type Log struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Previous is the last terminated instance (what a crash printed).
	Previous bool   `json:"previous,omitempty"`
	Text     string `json:"text"`
}

// Sanitize strips what diagnosis never needs and must not carry: managed
// fields, last-applied annotations and environment values (names stay). It
// redacts event and termination messages with r. Collect calls it; it is
// idempotent.
func (s *Snapshot) Sanitize(r *redact.Redactor) {
	// Compare the original specs before erasing values and unused fields.
	// Otherwise an obsolete rollout pod whose only difference is an env
	// value, probe credential or removed spec field could become identical
	// to the current template after sanitization and diagnose its successor.
	s.Pods = slices.DeleteFunc(s.Pods, func(p corev1.Pod) bool {
		return !s.matchesCurrentTemplate(&p)
	})
	for i := range s.Pods {
		p := &s.Pods[i]
		sanitizeMeta(r, &p.ObjectMeta)
		sanitizePodSpec(r, &p.Spec)
		for _, sts := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for j := range sts {
				sts[j].Image = r.String(sts[j].Image)
				sts[j].ImageID = r.String(sts[j].ImageID)
				redactState(r, &sts[j].State)
				redactState(r, &sts[j].LastTerminationState)
			}
		}
		p.Status.Message = r.String(p.Status.Message)
		p.Status.EphemeralContainerStatuses = nil
		for j := range p.Status.Conditions {
			p.Status.Conditions[j].Message = r.String(p.Status.Conditions[j].Message)
		}
	}
	for i := range s.Events {
		sanitizeMeta(r, &s.Events[i].ObjectMeta)
		s.Events[i].Message = r.String(s.Events[i].Message)
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		sanitizeMeta(r, &j.ObjectMeta)
		sanitizeMeta(r, &j.Spec.Template.ObjectMeta)
		sanitizePodSpec(r, &j.Spec.Template.Spec)
		for k := range j.Status.Conditions {
			j.Status.Conditions[k].Message = r.String(j.Status.Conditions[k].Message)
		}
	}
	for i := range s.Deployments {
		d := &s.Deployments[i]
		sanitizeMeta(r, &d.ObjectMeta)
		sanitizeMeta(r, &d.Spec.Template.ObjectMeta)
		sanitizePodSpec(r, &d.Spec.Template.Spec)
		for k := range d.Status.Conditions {
			d.Status.Conditions[k].Message = r.String(d.Status.Conditions[k].Message)
		}
	}
	for i := range s.StatefulSets {
		st := &s.StatefulSets[i]
		sanitizeMeta(r, &st.ObjectMeta)
		sanitizeMeta(r, &st.Spec.Template.ObjectMeta)
		sanitizePodSpec(r, &st.Spec.Template.Spec)
		st.Spec.VolumeClaimTemplates = nil
	}
	for i := range s.Services {
		sanitizeMeta(r, &s.Services[i].ObjectMeta)
		sp := &s.Services[i].Spec
		*sp = corev1.ServiceSpec{Selector: diagnosisLabels(sp.Selector), ClusterIP: sp.ClusterIP, Ports: sp.Ports, Type: sp.Type}
	}
	for i := range s.EndpointSlices {
		sanitizeMeta(r, &s.EndpointSlices[i].ObjectMeta)
	}
	for i := range s.Quotas {
		sanitizeMeta(r, &s.Quotas[i].ObjectMeta)
	}
	for i := range s.Routes {
		sanitizeMeta(r, &s.Routes[i].ObjectMeta)
		for j, h := range s.Routes[i].Spec.Hostnames {
			s.Routes[i].Spec.Hostnames[j] = gatewayv1.Hostname(r.String(string(h)))
		}
		for j := range s.Routes[i].Spec.Rules {
			rule := &s.Routes[i].Spec.Rules[j]
			*rule = gatewayv1.HTTPRouteRule{BackendRefs: rule.BackendRefs}
			for k := range rule.BackendRefs {
				rule.BackendRefs[k].Filters = nil
			}
		}
		for j := range s.Routes[i].Status.Parents {
			for k := range s.Routes[i].Status.Parents[j].Conditions {
				c := &s.Routes[i].Status.Parents[j].Conditions[k]
				c.Message = r.String(c.Message)
			}
		}
	}
	if s.Failure != nil {
		s.Failure.Message = r.String(s.Failure.Message)
	}
	for i := range s.Logs {
		s.Logs[i].Text = r.String(s.Logs[i].Text)
	}
	for i := range s.ConfigProblems {
		s.ConfigProblems[i].Message = r.String(s.ConfigProblems[i].Message)
	}
	s.Notes = redactAll(r, s.Notes)
}

func sanitizeMeta(r *redact.Redactor, m *metav1.ObjectMeta) {
	m.ManagedFields = nil
	delete(m.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	for k, v := range m.Annotations {
		m.Annotations[k] = r.String(v)
	}
	// These labels are public object identities or bounded structural values.
	// Redacting them would break joins and generation fencing when a short
	// secret happens to be "1", "service", or a workload's own name. Omit
	// unrelated labels rather than carrying arbitrary app metadata.
	m.Labels = diagnosisLabels(m.Labels)
}

// Keep only the pod-spec fields the rules use. Inline volume contents,
// lifecycle hooks, security settings and other app-controlled payloads do
// not explain a failure and can carry credentials in arbitrary strings.
func sanitizePodSpec(r *redact.Redactor, p *corev1.PodSpec) {
	for _, cs := range [][]corev1.Container{p.InitContainers, p.Containers} {
		for i := range cs {
			c := &cs[i]
			for j := range c.Env {
				c.Env[j].Value = ""
			}
			*c = corev1.Container{Name: c.Name, Image: r.String(c.Image), Command: redactAll(r, c.Command), Args: redactAll(r, c.Args),
				Ports: c.Ports, Env: c.Env, Resources: c.Resources, ReadinessProbe: c.ReadinessProbe,
				LivenessProbe: c.LivenessProbe, StartupProbe: c.StartupProbe}
			for _, probe := range []*corev1.Probe{c.ReadinessProbe, c.LivenessProbe, c.StartupProbe} {
				if probe == nil {
					continue
				}
				if probe.Exec != nil {
					probe.Exec.Command = redactAll(r, probe.Exec.Command)
				}
				if probe.HTTPGet != nil {
					probe.HTTPGet.Path = r.String(probe.HTTPGet.Path)
					probe.HTTPGet.Host = r.String(probe.HTTPGet.Host)
					// Authentication headers are values, never diagnostic data.
					probe.HTTPGet.HTTPHeaders = nil
				}
				if probe.TCPSocket != nil {
					probe.TCPSocket.Host = r.String(probe.TCPSocket.Host)
				}
				if probe.GRPC != nil && probe.GRPC.Service != nil {
					v := r.String(*probe.GRPC.Service)
					probe.GRPC.Service = &v
				}
			}
		}
	}
	*p = corev1.PodSpec{InitContainers: p.InitContainers, Containers: p.Containers}
}

// OmitSensitiveText fails closed when the collector cannot read the injected
// values. Structured reasons, codes and identities remain usable, but app
// output and free-form object payloads cannot leave the cluster unredacted.
// Call after Sanitize, which has already removed unused spec fields.
func (s *Snapshot) OmitSensitiveText() {
	var metadata []*metav1.ObjectMeta
	for i := range s.Pods {
		p := &s.Pods[i]
		metadata = append(metadata, &p.ObjectMeta)
		omitPodText(&p.Spec)
		p.Status.Message = ""
		for j := range p.Status.Conditions {
			p.Status.Conditions[j].Message = ""
		}
		for _, sts := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for j := range sts {
				sts[j].Image, sts[j].ImageID = "", ""
				for _, state := range []*corev1.ContainerState{&sts[j].State, &sts[j].LastTerminationState} {
					if state.Waiting != nil {
						state.Waiting.Message = ""
					}
					if state.Terminated != nil {
						state.Terminated.Message = ""
					}
				}
			}
		}
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		metadata = append(metadata, &j.ObjectMeta, &j.Spec.Template.ObjectMeta)
		omitPodText(&j.Spec.Template.Spec)
		for k := range j.Status.Conditions {
			j.Status.Conditions[k].Message = ""
		}
	}
	for i := range s.Deployments {
		d := &s.Deployments[i]
		metadata = append(metadata, &d.ObjectMeta, &d.Spec.Template.ObjectMeta)
		omitPodText(&d.Spec.Template.Spec)
		for k := range d.Status.Conditions {
			d.Status.Conditions[k].Message = ""
		}
	}
	for i := range s.StatefulSets {
		st := &s.StatefulSets[i]
		metadata = append(metadata, &st.ObjectMeta, &st.Spec.Template.ObjectMeta)
		omitPodText(&st.Spec.Template.Spec)
	}
	for i := range s.Events {
		metadata = append(metadata, &s.Events[i].ObjectMeta)
		s.Events[i].Message = ""
	}
	for i := range s.Services {
		metadata = append(metadata, &s.Services[i].ObjectMeta)
		s.Services[i].Spec.Selector = diagnosisLabels(s.Services[i].Spec.Selector)
	}
	for i := range s.EndpointSlices {
		metadata = append(metadata, &s.EndpointSlices[i].ObjectMeta)
	}
	for i := range s.Quotas {
		metadata = append(metadata, &s.Quotas[i].ObjectMeta)
	}
	for i := range s.Routes {
		r := &s.Routes[i]
		metadata = append(metadata, &r.ObjectMeta)
		for j := range r.Status.Parents {
			for k := range r.Status.Parents[j].Conditions {
				r.Status.Parents[j].Conditions[k].Message = ""
			}
		}
	}
	for _, m := range metadata {
		m.Annotations = nil
		m.Labels = diagnosisLabels(m.Labels)
	}
	if s.Failure != nil {
		s.Failure.Message = ""
	}
	for i := range s.ConfigProblems {
		s.ConfigProblems[i].Message = ""
	}
	s.Logs = nil
	s.Notes = []string{"logs omitted: the preview's secrets could not be read; other sensitive text was removed too"}
}

func omitPodText(p *corev1.PodSpec) {
	for _, cs := range [][]corev1.Container{p.InitContainers, p.Containers} {
		for i := range cs {
			c := &cs[i]
			c.Command, c.Args, c.Image = nil, nil, ""
			for _, probe := range []*corev1.Probe{c.ReadinessProbe, c.LivenessProbe, c.StartupProbe} {
				if probe == nil {
					continue
				}
				probe.Exec = nil
				if probe.HTTPGet != nil {
					probe.HTTPGet.Path, probe.HTTPGet.Host = "", ""
				}
				if probe.TCPSocket != nil {
					probe.TCPSocket.Host = ""
				}
				if probe.GRPC != nil {
					probe.GRPC.Service = nil
				}
			}
		}
	}
}

func diagnosisLabels(labels map[string]string) map[string]string {
	var out map[string]string
	for _, k := range []string{labelName, labelComponent, labelJobName, render.LabelStage, render.LabelGeneration,
		"kubernetes.io/service-name", "pod-template-hash"} {
		if v, ok := labels[k]; ok {
			switch k {
			case render.LabelGeneration:
				if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
					v = "invalid" // still fenced out; never expose its arbitrary payload
				}
			case render.LabelStage:
				if stageIndex(v) < 0 {
					continue
				}
			case labelComponent:
				if _, valid := stageOfComponent[v]; !valid {
					continue
				}
			}
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func redactAll(r *redact.Redactor, in []string) []string {
	for i := range in {
		in[i] = r.String(in[i])
	}
	return in
}

func redactState(r *redact.Redactor, s *corev1.ContainerState) {
	if s.Waiting != nil {
		s.Waiting.Message = r.String(s.Waiting.Message)
	}
	if s.Terminated != nil {
		s.Terminated.Message = r.String(s.Terminated.Message)
	}
}

// Label values the renderer puts on pods (internal/render/labels.go).
const (
	labelName      = "app.kubernetes.io/name"
	labelComponent = "app.kubernetes.io/component"
	labelJobName   = "batch.kubernetes.io/job-name"
)

// Stages in pipeline order.
var stages = []string{string(render.StageGuardrails), string(render.StageDependencies), string(render.StageBaselineDB),
	string(render.StageApplication), string(render.StageSmoke)}

// stageOfComponent maps app.kubernetes.io/component to the stage that runs it.
var stageOfComponent = map[string]string{
	"guardrail": string(render.StageGuardrails),
	"database":  string(render.StageDependencies), "cache": string(render.StageDependencies), "broker": string(render.StageDependencies),
	"database-admin": string(render.StageBaselineDB), "migration": string(render.StageBaselineDB), "seed": string(render.StageBaselineDB),
	"service": string(render.StageApplication), "worker": string(render.StageApplication), "route": string(render.StageApplication),
	"smoke-test": string(render.StageSmoke),
}

// dependencyComponents run the preview's own PostgreSQL, Redis and RabbitMQ.
var dependencyComponents = map[string]bool{"database": true, "cache": true, "broker": true}

func stageIndex(stage string) int {
	for i, s := range stages {
		if s == stage {
			return i
		}
	}
	return -1
}

// workload describes what a pod, Job or workload object is, from its labels.
type workload struct {
	name      string // app.kubernetes.io/name
	component string
	stage     string
}

func workloadOf(labels map[string]string) workload {
	w := workload{name: labels[labelName], component: labels[labelComponent]}
	w.stage = labels[render.LabelStage]
	if w.stage == "" {
		w.stage = stageOfComponent[w.component]
	}
	return w
}

// current reports whether labels belong to the snapshot's generation. Objects
// without a generation label (long-running workloads' pods) are current.
func (s *Snapshot) current(labels map[string]string) bool {
	g, ok := labels[render.LabelGeneration]
	if !ok || s.Generation == 0 {
		return true
	}
	n, err := strconv.ParseInt(g, 10, 64)
	return err == nil && n == s.Generation
}

// logFor returns the log tail of a container, preferring the previous
// instance when asked for (a crash) and falling back to the other.
func (s *Snapshot) logFor(pod, container string, previous bool) string {
	var other string
	for _, l := range s.Logs {
		if l.Pod == pod && l.Container == container {
			if l.Previous == previous {
				return l.Text
			}
			other = l.Text
		}
	}
	return other
}

// lastLines returns the last n non-empty lines of text.
func lastLines(text string, n int) []string {
	out := outputLines(text)
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func outputLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		// The kubelet's own notice when a previous instance's log is gone is
		// not the app's output.
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "unable to retrieve container logs") {
			out = append(out, l)
		}
	}
	return out
}
