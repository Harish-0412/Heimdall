package diagnose

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heimdall-dev/heimdall/internal/render"
)

// index is the snapshot filtered to the current generation, with lookups.
// History (other generations' Jobs and their pods, terminating pods) and
// events about it never reach a rule, which keeps the root cause stable no
// matter what else the namespace has seen.
type index struct {
	pods         []corev1.Pod
	jobs         []batchv1.Job
	deployments  []appsv1.Deployment
	statefulSets []appsv1.StatefulSet
	events       map[string][]corev1.Event // Warning events by involved object name
	// workloads are the Deployment and StatefulSet names.
	workloads map[string]bool
}

func newIndex(s *Snapshot) *index {
	ix := &index{events: map[string][]corev1.Event{}, workloads: map[string]bool{}}
	currentJob := map[string]bool{}
	for _, j := range s.Jobs {
		if j.DeletionTimestamp == nil && s.current(j.Labels) && s.current(j.Spec.Template.Labels) {
			ix.jobs = append(ix.jobs, j)
			currentJob[j.Name] = true
		}
	}
	for _, p := range s.Pods {
		if p.DeletionTimestamp != nil || !s.current(p.Labels) || !s.matchesCurrentTemplate(&p) {
			continue
		}
		if job := ownerOf(&p, "Job"); job != "" && !currentJob[job] {
			continue
		}
		ix.pods = append(ix.pods, p)
	}
	known := map[string]metav1.ObjectMeta{}
	replicaSets := map[string]metav1.OwnerReference{}
	workloadHasReplicaSet := map[string]bool{}
	for _, p := range ix.pods {
		known["Pod/"+p.Name] = p.ObjectMeta
		for _, o := range p.OwnerReferences {
			if o.Kind == "ReplicaSet" {
				replicaSets[o.Name] = o
			}
		}
	}
	for _, j := range ix.jobs {
		known["Job/"+j.Name] = j.ObjectMeta
	}
	for _, d := range s.Deployments {
		if d.DeletionTimestamp != nil || !s.current(d.Labels) {
			continue
		}
		ix.deployments = append(ix.deployments, d)
		known["Deployment/"+d.Name] = d.ObjectMeta
		ix.workloads[d.Name] = true
	}
	for _, st := range s.StatefulSets {
		if st.DeletionTimestamp != nil || !s.current(st.Labels) {
			continue
		}
		ix.statefulSets = append(ix.statefulSets, st)
		known["StatefulSet/"+st.Name] = st.ObjectMeta
		ix.workloads[st.Name] = true
	}
	for name := range replicaSets {
		workloadHasReplicaSet[ix.deploymentOfReplicaSet(name)] = true
	}
	for _, e := range s.Events {
		if e.Type != corev1.EventTypeWarning || !s.current(e.Labels) {
			continue
		}
		name := e.InvolvedObject.Name
		kind := e.InvolvedObject.Kind
		if e.InvolvedObject.Kind == "ReplicaSet" {
			name = ix.deploymentOfReplicaSet(name)
			owner, ok := replicaSets[e.InvolvedObject.Name]
			if workloadHasReplicaSet[name] && (!ok || (owner.UID != "" && e.InvolvedObject.UID != "" && owner.UID != e.InvolvedObject.UID)) {
				continue
			}
			kind = "Deployment"
		}
		m, ok := known[kind+"/"+name]
		if !ok || (!m.CreationTimestamp.IsZero() && eventTime(e).Before(m.CreationTimestamp.Time)) {
			continue
		}
		if e.InvolvedObject.Kind != "ReplicaSet" && m.UID != "" && e.InvolvedObject.UID != "" && m.UID != e.InvolvedObject.UID {
			continue
		}
		ix.events[name] = append(ix.events[name], e)
	}
	return ix
}

// Long-lived workload pods deliberately carry no deployment generation.
// During a rollout, compare them with the workload's accepted template so
// an old ReplicaSet's crash cannot diagnose a generation that has replaced
// its image or configuration. Older snapshots omit template specs, in
// which case there is no authoritative template to compare.
func (s *Snapshot) matchesCurrentTemplate(p *corev1.Pod) bool {
	var template *corev1.PodTemplateSpec
	if owner := ownerOf(p, "ReplicaSet"); owner != "" {
		for i := range s.Deployments {
			d := &s.Deployments[i]
			if sep := strings.LastIndexByte(owner, '-'); sep > 0 && d.Name == owner[:sep] {
				if d.DeletionTimestamp != nil || !s.current(d.Labels) {
					return false
				}
				template = &d.Spec.Template
				break
			}
		}
	} else if owner := ownerOf(p, "StatefulSet"); owner != "" {
		for i := range s.StatefulSets {
			st := &s.StatefulSets[i]
			if st.Name == owner {
				if st.DeletionTimestamp != nil || !s.current(st.Labels) {
					return false
				}
				template = &st.Spec.Template
				break
			}
		}
	}
	return template == nil || len(template.Spec.Containers) == 0 ||
		(labelsMatch(template.Labels, p.Labels) && PodSpecMatchesTemplate(template.Spec, p.Spec))
}

// deploymentOfReplicaSet strips the pod-template hash: "api-6d4f9c7b8" -> "api".
func (ix *index) deploymentOfReplicaSet(rs string) string {
	if i := strings.LastIndexByte(rs, '-'); i > 0 && ix.workloads[rs[:i]] {
		return rs[:i]
	}
	return rs
}

func ownerOf(p *corev1.Pod, kind string) string {
	for _, o := range p.OwnerReferences {
		if o.Kind == kind {
			return o.Name
		}
	}
	return ""
}

// about describes a pod as the subject of a diagnosis.
func (ix *index) about(p *corev1.Pod) Diagnosis {
	w := workloadOf(p.Labels)
	d := Diagnosis{Workload: w.name, Stage: w.stage, component: w.component, Subject: "pod/" + p.Name}
	switch {
	case ownerOf(p, "ReplicaSet") != "":
		d.Subject = "deployment/" + ix.deploymentOfReplicaSet(ownerOf(p, "ReplicaSet"))
	case ownerOf(p, "StatefulSet") != "":
		d.Subject = "statefulset/" + ownerOf(p, "StatefulSet")
	case ownerOf(p, "Job") != "":
		d.Subject = "job/" + ownerOf(p, "Job")
	}
	if d.Workload == "" {
		_, d.Workload, _ = strings.Cut(d.Subject, "/")
	}
	return d
}

func (ix *index) podEvents(p *corev1.Pod, reasons ...string) []corev1.Event {
	var out []corev1.Event
	for _, e := range ix.events[p.Name] {
		if len(reasons) == 0 || slices.Contains(reasons, e.Reason) {
			out = append(out, e)
		}
	}
	return out
}

func eventLines(es []corev1.Event) []string {
	var out []string
	for _, e := range es {
		line := e.Reason + ": " + e.Message
		if e.Count > 1 {
			line += fmt.Sprintf(" (x%d)", e.Count)
		}
		out = append(out, line)
	}
	return out
}

func statuses(p *corev1.Pod) []corev1.ContainerStatus {
	return append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...)
}

func containerSpec(p *corev1.Pod, name string) *corev1.Container {
	for _, cs := range [][]corev1.Container{p.Spec.InitContainers, p.Spec.Containers} {
		for i := range cs {
			if cs[i].Name == name {
				return &cs[i]
			}
		}
	}
	return nil
}

// --- configuration and policy ------------------------------------------------

var stateCodes = map[string]bool{"engine.stale_generation": true, "engine.stale_reset": true, "engine.generation_conflict": true}

func configRule(s *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	var invalid, denied []string
	for _, p := range s.ConfigProblems {
		line := p.Code + ": " + p.Message
		if p.Path != "" {
			line = p.Path + ": " + line
		}
		if p.Line > 0 {
			line = fmt.Sprintf("line %d: %s", p.Line, line)
		}
		if strings.HasPrefix(p.Code, "policy.") {
			denied = append(denied, line)
		} else {
			invalid = append(invalid, line)
		}
	}
	if len(denied) > 0 {
		out = append(out, Diagnosis{Code: PolicyDenied, Summary: denied[0], Subject: "heimdall.yaml", Evidence: denied,
			Suggestion: "The configuration asks for more than this tenant's policy allows. Reduce it to fit, or ask a " +
				"platform administrator to change the policy. `heimdall validate` shows the same errors locally."})
	}
	if len(invalid) > 0 {
		out = append(out, Diagnosis{Code: ConfigInvalid, Summary: invalid[0], Subject: "heimdall.yaml", Evidence: invalid,
			Suggestion: "Fix heimdall.yaml; `heimdall validate` reports the same errors, with hints, before you push."})
	}
	if f := s.Failure; f != nil && len(out) == 0 {
		switch {
		case stateCodes[f.Code]:
			d := Diagnosis{Code: StaleGeneration, Severity: SeverityWarning, Summary: f.Message, Subject: "generation",
				Suggestion: "A newer push superseded this one; its results were discarded on purpose. Nothing to fix: " +
					"look at the newest generation."}
			if s.AcceptedGeneration > s.Generation && s.Generation > 0 {
				d.Summary = fmt.Sprintf("generation %d is older than generation %d, which this preview already accepted",
					s.Generation, s.AcceptedGeneration)
				d.Subject = "generation/" + strconv.FormatInt(s.Generation, 10)
			}
			out = append(out, d)
		case f.Code == "agent.config_invalid" || f.Code == "agent.config_digest" || f.Code == "agent.data_missing" ||
			f.Code == "agent.data_unexpected" || f.Code == "engine.spec_invalid":
			d := Diagnosis{Code: ConfigInvalid, Summary: f.Message, Subject: "heimdall.yaml",
				Suggestion: "Fix heimdall.yaml (`heimdall validate` shows the errors) and push again."}
			if strings.Contains(f.Message, "render.image") {
				d.Suggestion = "Every service and worker needs an image pinned by digest (name@sha256:...): make CI " +
					"build and push each image and pass its digest."
			}
			if strings.Contains(f.Message, "policy") {
				d.Code = PolicyDenied
				d.Suggestion = "The configuration asks for more than this tenant's policy allows. Reduce it, or ask a " +
					"platform administrator to change the policy."
			}
			out = append(out, d)
		case strings.HasPrefix(f.Code, "engine.data_") || strings.HasPrefix(f.Code, "agent.data_"):
			out = append(out, Diagnosis{Code: PolicyDenied, Summary: f.Message, Subject: "data import",
				Suggestion: "Imports must be operator-approved and sanitised, and match the approved SHA-256 exactly " +
					"(docs/engine.md). Re-approve the exact bytes, or remove the import."})
		case f.Code == "engine.ownership":
			out = append(out, Diagnosis{Code: PolicyDenied, Summary: f.Message, Subject: "namespace",
				Suggestion: "An object Heimdall needs to manage belongs to someone else. Remove it, or let Heimdall own it."})
		}
	}
	// Admission rejections of pods (Pod Security, webhooks) surface as
	// FailedCreate events on their controllers.
	for name, es := range ix.events {
		for _, e := range es {
			if e.Reason == "FailedCreate" && (strings.Contains(e.Message, "violates PodSecurity") ||
				strings.Contains(e.Message, "admission webhook") || strings.Contains(e.Message, "ValidatingAdmissionPolicy")) {
				out = append(out, Diagnosis{Code: PolicyDenied, Summary: name + ": " + firstLine(e.Message), Workload: name,
					Subject: strings.ToLower(e.InvolvedObject.Kind) + "/" + name, Evidence: eventLines([]corev1.Event{e}),
					Suggestion: "The cluster's admission policy rejected the pods. Heimdall renders restricted pods; " +
						"a platform policy stricter than that must be relaxed for previews, or the workload changed."})
			}
		}
	}
	return out
}

// --- images ------------------------------------------------------------------

var pullWaiting = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true}

func imagePullRule(_ *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for i := range ix.pods {
		p := &ix.pods[i]
		for _, cs := range statuses(p) {
			if cs.State.Waiting == nil || !pullWaiting[cs.State.Waiting.Reason] {
				continue
			}
			d := ix.about(p)
			d.Code = ImagePullFailed
			evs := ix.podEvents(p, "Failed")
			cause := cs.State.Waiting.Message
			for _, e := range evs {
				if strings.Contains(e.Message, "pull") || strings.Contains(e.Message, "image") {
					cause = e.Message
				}
			}
			d.Summary = fmt.Sprintf("%s cannot pull image %s", d.who(), cs.Image)
			d.Evidence = append([]string{cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message}, eventLines(evs)...)
			lc := strings.ToLower(cause)
			switch {
			case cs.State.Waiting.Reason == "InvalidImageName":
				d.Summary += ": the reference is invalid"
				d.Suggestion = "Use a valid, digest-pinned reference (registry/name@sha256:<64 hex>)."
			case strings.Contains(lc, "not found") || strings.Contains(lc, "manifest unknown") || strings.Contains(lc, "notfound"):
				d.Summary += ": it does not exist in the registry"
				d.Suggestion = "The digest is not in the registry. Make sure CI pushed the image before requesting the " +
					"preview, and that the digest and repository name match what it pushed."
			case strings.Contains(lc, "unauthorized") || strings.Contains(lc, "authentication required") ||
				strings.Contains(lc, "denied") || strings.Contains(lc, "403") || strings.Contains(lc, "401"):
				d.Summary += ": the registry refused access"
				d.Suggestion = "The cluster cannot authenticate to this registry. Grant the cluster's nodes pull access " +
					"(or configure an image mirror the platform trusts); previews get no pull secrets of their own."
			case strings.Contains(lc, "no such host") || strings.Contains(lc, "i/o timeout") ||
				strings.Contains(lc, "connection refused") || strings.Contains(lc, "dial tcp"):
				d.Summary += ": the registry is unreachable"
				d.Suggestion = "The cluster cannot reach this registry. Check the registry host, the cluster's egress, " +
					"or the platform's image mirror."
			default:
				d.Suggestion = "Check that the image exists and that the cluster may pull it; the evidence below has " +
					"the registry's answer."
			}
			out = append(out, d)
		}
	}
	return out
}

// --- scheduling --------------------------------------------------------------

var insufficient = regexp.MustCompile(`Insufficient ([a-z][a-z0-9./-]*[a-z0-9])`)

func schedulingRule(_ *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for i := range ix.pods {
		p := &ix.pods[i]
		for _, c := range p.Status.Conditions {
			if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
				continue
			}
			d := ix.about(p)
			d.Code = NoCapacity
			d.Evidence = append([]string{c.Message}, eventLines(ix.podEvents(p, "FailedScheduling"))...)
			m := c.Message
			switch {
			case strings.Contains(m, "Insufficient"):
				res := insufficient.FindAllStringSubmatch(m, -1)
				var names []string
				for _, r := range res {
					names = append(names, r[1])
				}
				slices.Sort(names)
				names = slices.Compact(names)
				d.Summary = fmt.Sprintf("%s cannot be scheduled: no node has enough %s", d.who(), strings.Join(names, " or "))
				d.Suggestion = "The cluster is full for this request. Lower `resources` for " + d.Workload +
					" in heimdall.yaml, or wait: the platform's autoscaler may add a node."
			case strings.Contains(m, "unbound") && strings.Contains(m, "PersistentVolumeClaim"):
				d.Summary = d.who() + " cannot be scheduled: its volume is not provisioned"
				d.Suggestion = "The storage class cannot provision the database volume. A platform administrator " +
					"should check the storage class and its provisioner."
			case strings.Contains(m, "affinity") || strings.Contains(m, "selector") || strings.Contains(m, "taint"):
				d.Summary = d.who() + " cannot be scheduled: no node matches the preview node pool"
				d.Suggestion = "The platform pins previews to a node pool (platform.nodeSelector/tolerations) that has " +
					"no matching, untainted node. A platform administrator should check the pool."
			default:
				d.Summary = d.who() + " cannot be scheduled: " + firstLine(m)
				d.Suggestion = "No node can run it now. Check the evidence, and the cluster's capacity."
			}
			out = append(out, d)
		}
	}
	return out
}

// --- quota -------------------------------------------------------------------

var quotaPattern = regexp.MustCompile(`exceeded quota: ([^,]+), requested: ([^,]+(?:,[^:,]+=[^,]+)*), used: ([^,]+(?:,[^:,]+=[^,]+)*), limited: (.+)$`)

func quotaRule(s *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	add := func(kind, name, message string, ev []string) {
		w := name
		d := Diagnosis{Code: QuotaExceeded, Workload: w, Subject: strings.ToLower(kind) + "/" + name, Evidence: ev}
		for _, dep := range ix.deployments {
			if dep.Name == name {
				wl := workloadOf(dep.Labels)
				d.Stage, d.component = wl.stage, wl.component
			}
		}
		for _, st := range ix.statefulSets {
			if st.Name == name {
				wl := workloadOf(st.Labels)
				d.Stage, d.component = wl.stage, wl.component
			}
		}
		for _, j := range ix.jobs {
			if j.Name == name {
				wl := workloadOf(j.Spec.Template.Labels)
				d.Stage, d.component = wl.stage, wl.component
			}
		}
		d.Summary = name + " cannot create pods: the preview's resource quota is exhausted"
		named := map[string]bool{} // the resources the rejection names
		if m := quotaPattern.FindStringSubmatch(message); m != nil {
			d.Summary += fmt.Sprintf(" (requested %s; used %s of %s)", m[2], m[3], m[4])
			for _, kv := range strings.Split(m[2], ",") {
				res, _, _ := strings.Cut(strings.TrimSpace(kv), "=")
				named[res] = true
			}
		}
		d.Suggestion = "The preview's quota is sized from heimdall.yaml, so this happens when something adds pods beyond " +
			"it (a manual scale, extra replicas, a stuck rollout keeping old pods). Remove the extra pods, or raise " +
			"`resources` in heimdall.yaml within the tenant's limits."
		for _, q := range s.Quotas {
			if q.DeletionTimestamp != nil || !s.current(q.Labels) {
				continue
			}
			for _, res := range slices.Sorted(maps.Keys(q.Status.Hard)) {
				hard := q.Status.Hard[res]
				// The resources the rejection names, or, without that, any
				// with a non-zero limit that is used up.
				relevant := named[string(res)] || (len(named) == 0 && !hard.IsZero())
				if used, ok := q.Status.Used[res]; ok && relevant && (len(named) > 0 || used.Cmp(hard) >= 0) {
					d.Evidence = append(d.Evidence, fmt.Sprintf("quota %s: %s used %s of %s", q.Name, res, used.String(), hard.String()))
				}
			}
		}
		out = append(out, d)
	}
	seen := map[string]bool{}
	for name, es := range ix.events {
		for _, e := range es {
			if e.Reason == "FailedCreate" && strings.Contains(e.Message, "exceeded quota") && !seen[name] {
				seen[name] = true
				kind := e.InvolvedObject.Kind
				if kind == "ReplicaSet" {
					kind = "Deployment"
				}
				add(kind, name, e.Message, eventLines([]corev1.Event{e}))
			}
		}
	}
	for _, dep := range ix.deployments {
		for _, c := range dep.Status.Conditions {
			if c.Type == "ReplicaFailure" && c.Status == corev1.ConditionTrue && strings.Contains(c.Message, "exceeded quota") && !seen[dep.Name] {
				seen[dep.Name] = true
				add("Deployment", dep.Name, c.Message, []string{c.Reason + ": " + c.Message})
			}
		}
	}
	slices.SortFunc(out, func(a, b Diagnosis) int { return strings.Compare(a.Subject, b.Subject) })
	return out
}

// --- containers: crashes, out of memory, configuration errors ---------------

var exitHints = map[int32]string{
	126: "the command is not executable",
	127: "the command was not found in the image",
	137: "it was killed (SIGKILL)",
	139: "it crashed with a segmentation fault",
	143: "it was terminated (SIGTERM) and did not exit cleanly",
}

var errorLinePatterns = regexp.MustCompile(`(?i)(missing required|\berror\b|exception|panic:|fatal|traceback|cannot find module|refused|not found|failed)`)

// firstError is the first line in a log tail that looks like the error.
func firstError(text string) string {
	for _, l := range outputLines(text) {
		if diagnosticErrorLine(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func diagnosticErrorLine(l string) bool {
	return errorLinePatterns.MatchString(l) && !strings.HasPrefix(strings.TrimSpace(l), "at ") && !isErrorDumpLine(l)
}

func containerRule(s *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for i := range ix.pods {
		p := &ix.pods[i]
		// A Job's exits are the Job's failure (jobRule); a Job pod that
		// cannot even start never fails its Job, so it is explained here.
		job := ownerOf(p, "Job") != ""
		liveness := ix.podEvents(p, "Unhealthy")
		killedByProbe := slices.ContainsFunc(liveness, func(e corev1.Event) bool { return strings.HasPrefix(e.Message, "Liveness probe failed") })
		for _, cs := range statuses(p) {
			if cs.Ready && cs.State.Running != nil {
				continue // a previous crash or OOM has recovered
			}
			term := cs.LastTerminationState.Terminated
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				term = cs.State.Terminated
			}
			waiting := cs.State.Waiting
			d := ix.about(p)
			switch {
			case !job && term != nil && term.Reason == "OOMKilled":
				d.Code = OutOfMemory
				limit := "its memory limit"
				if c := containerSpec(p, cs.Name); c != nil {
					if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
						limit = "its " + q.String() + " memory limit"
					}
				}
				d.Summary = fmt.Sprintf("%s was killed for exceeding %s (%d restarts)", d.who(), limit, cs.RestartCount)
				d.Suggestion = "Raise `resources.memory` for " + d.Workload + " in heimdall.yaml, or find what makes it " +
					"use more memory than in production (a cache without a bound, loading a whole table)."
				d.Evidence = append(d.Evidence, fmt.Sprintf("container %s: OOMKilled, exit code %d", cs.Name, term.ExitCode))
				d.Evidence = append(d.Evidence, lastLines(s.logFor(p.Name, cs.Name, true), 5)...)
			case waiting != nil && (waiting.Reason == "CreateContainerConfigError" || waiting.Reason == "CreateContainerError"):
				d.Code = ConfigInvalid
				d.Summary = fmt.Sprintf("%s cannot start: %s", d.who(), waiting.Message)
				d.Evidence = append(d.Evidence, waiting.Reason+": "+waiting.Message)
				d.Suggestion = "The container's configuration refers to something that does not exist."
				if keys := missingSecrets(containerSpec(p, cs.Name), waiting.Message, s.MissingSecrets...); len(keys) > 0 {
					names := "`" + strings.Join(keys, "`, `") + "`"
					d.Summary = fmt.Sprintf("%s cannot start: it needs the secret %s, which this tenant has not configured", d.who(), names)
					if slices.Contains(s.MissingSecrets, render.AppSecretsSecret) {
						d.Evidence = append(d.Evidence, "Secret "+render.AppSecretsSecret+" not found (API lookup)")
					}
					where := ""
					if !job {
						where = " for " + d.Workload
					}
					d.Suggestion = "heimdall.yaml lists " + names + " under `secrets:`" + where + ", but the platform delivers " +
						"no such secret to this preview. Add it to the tenant's secrets, or remove it from `secrets:`."
				} else if strings.Contains(waiting.Message, "secret") {
					d.Suggestion = "A secret the configuration declares is missing. Add it to the tenant's secrets " +
						"(or remove it from heimdall.yaml)."
				}
			case !job && term != nil && term.ExitCode != 0 && (cs.RestartCount > 0 || (waiting != nil && waiting.Reason == "CrashLoopBackOff")):
				if killedByProbe && (term.ExitCode == 137 || term.ExitCode == 143) {
					continue // the liveness probe killed it: healthRule
				}
				d.Code = ContainerCrash
				d.Summary = fmt.Sprintf("%s exits with code %d", d.who(), term.ExitCode)
				if hint := exitHints[term.ExitCode]; hint != "" {
					d.Summary += " (" + hint + ")"
				}
				d.Summary += fmt.Sprintf(", %d restarts", cs.RestartCount)
				logs := s.logFor(p.Name, cs.Name, true)
				if len(lastLines(logs, 1)) == 0 {
					// The log is gone (the kubelet keeps one previous instance);
					// the termination message is the tail of the same output
					// (terminationMessagePolicy FallbackToLogsOnError).
					logs = term.Message
				}
				if line := firstError(logs); line != "" {
					d.Summary += ": " + line
				} else if term.Message != "" {
					d.Summary += ": " + firstLine(term.Message)
				}
				d.Suggestion = crashSuggestion(d.Workload, term.ExitCode, logs)
				d.Evidence = append(d.Evidence, fmt.Sprintf("container %s: %s, exit code %d", cs.Name, term.Reason, term.ExitCode))
				d.Evidence = append(d.Evidence, crashEvidence(logs)...)
			case waiting != nil && waiting.Reason == "RunContainerError":
				d.Code = ContainerCrash
				d.Summary = fmt.Sprintf("%s cannot start: %s", d.who(), firstLine(waiting.Message))
				d.Evidence = append(d.Evidence, waiting.Reason+": "+waiting.Message)
				d.Suggestion = "The container runtime could not start the command. Check `command:` in heimdall.yaml and " +
					"the image's entrypoint."
			default:
				continue
			}
			out = append(out, d)
		}
	}
	return out
}

// crashEvidence is what a crash printed from its first error on, without
// stack frames, or its last lines when nothing looks like an error.
func crashEvidence(logs string) []string {
	lines := outputLines(logs)
	first := slices.IndexFunc(lines, diagnosticErrorLine)
	if first < 0 {
		return lastLines(logs, 8)
	}
	out := slices.DeleteFunc(slices.Clone(lines[first:]), func(l string) bool {
		return strings.HasPrefix(strings.TrimSpace(l), "at ")
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

var missingEnv = regexp.MustCompile(`missing required environment variable (\w+)`)

func crashSuggestion(workload string, code int32, logs string) string {
	if m := missingEnv.FindStringSubmatch(logs); m != nil {
		return "It needs `" + m[1] + "`, which is not set. Add it under `env:` (or `secrets:`) for " + workload + " in heimdall.yaml."
	}
	switch code {
	case 126, 127:
		return "Check `command:` for " + workload + " in heimdall.yaml against what the image contains."
	}
	return "It starts and then exits. The last lines it printed are below; reproduce with `docker run` of the same " +
		"image and environment, or read more with `heimdall logs --workload " + workload + "`."
}

var missingKey = regexp.MustCompile(`couldn't find key (\S+) in Secret`)

// missingSecrets names the tenant secrets a container cannot get: the key
// the kubelet names, or every key it takes from the platform-delivered
// Secret when that Secret does not exist at all.
func missingSecrets(c *corev1.Container, message string, confirmedMissing ...string) []string {
	if m := missingKey.FindStringSubmatch(message); m != nil {
		return []string{m[1]}
	}
	if c == nil || (!strings.Contains(message, `"`+render.AppSecretsSecret+`" not found`) &&
		!slices.Contains(confirmedMissing, render.AppSecretsSecret)) {
		return nil
	}
	var keys []string
	for _, e := range c.Env {
		if r := e.ValueFrom; r != nil && r.SecretKeyRef != nil && r.SecretKeyRef.Name == render.AppSecretsSecret {
			keys = append(keys, r.SecretKeyRef.Key)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// --- database reachability ---------------------------------------------------

var dbErrors = []struct {
	re    *regexp.Regexp
	cause string
}{
	{regexp.MustCompile(`password authentication failed for user "([^"]+)"`), "auth"},
	{regexp.MustCompile(`no pg_hba\.conf entry`), "auth"},
	{regexp.MustCompile(`getaddrinfo (?:ENOTFOUND|EAI_AGAIN) ([A-Za-z0-9.-]+)`), "dns"},
	{regexp.MustCompile(`could not translate host name "([^"]+)"`), "dns"},
	{regexp.MustCompile(`ECONNREFUSED [0-9a-f.:\[\]]+:5432`), "refused"},
	{regexp.MustCompile(`(?i)connection to server at "[^"]*"(?: \([^)]*\))?, port 5432 failed`), "refused"},
	{regexp.MustCompile(`(?i)could not connect to server`), "refused"},
	{regexp.MustCompile(`the database system is (?:starting up|shutting down|in recovery mode)`), "starting"},
	{regexp.MustCompile(`(?i)(?:timeout expired|Connection terminated due to connection timeout).*`), "timeout"},
	// An established connection lost: node-postgres, PostgreSQL shutting
	// down, libpq.
	{regexp.MustCompile(`Connection terminated unexpectedly|terminating connection due to administrator command|server closed the connection unexpectedly`), "lost"},
}

func databaseRule(s *Snapshot, ix *index) []Diagnosis {
	dbReady, haveDB := true, false
	for _, p := range ix.pods {
		if p.Labels[labelComponent] == "database" {
			haveDB = true
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
					dbReady = false
				}
			}
		}
	}
	// PostgreSQL not running at all: its StatefulSet has no ready replica and
	// no pod (scaled down, or its pod deleted and not back). That is the
	// cause, reported once, about PostgreSQL, with every app that cannot
	// reach it as evidence; a pod that exists but is not ready has its own
	// diagnosis instead (scheduling, crash, ...).
	down := ""
	var db Diagnosis
	for _, st := range ix.statefulSets {
		if w := workloadOf(st.Labels); w.component == "database" && st.Status.ReadyReplicas == 0 && !haveDB {
			want := int32(1)
			if st.Spec.Replicas != nil {
				want = *st.Spec.Replicas
			}
			down = fmt.Sprintf("statefulset/%s has %d of %d replicas ready", st.Name, st.Status.ReadyReplicas, want)
			if want == 0 {
				down = "statefulset/" + st.Name + " is scaled to zero"
			}
			db = Diagnosis{Code: DBUnreachable, Subject: "statefulset/" + st.Name, Workload: st.Name, Stage: w.stage,
				component: w.component, Summary: "PostgreSQL is not running (" + down + ")", Evidence: []string{down},
				Suggestion: "PostgreSQL is down, not the apps that use it. `heimdall up` re-applies the preview and starts it " +
					"again; if it stops again, its own diagnosis names the cause."}
		}
	}
	var out, affected []Diagnosis
	for i := range ix.pods {
		p := &ix.pods[i]
		if dependencyComponents[p.Labels[labelComponent]] {
			continue
		}
		for _, cs := range statuses(p) {
			logs := s.logFor(p.Name, cs.Name, true) + "\n" + s.logFor(p.Name, cs.Name, false)
			for _, e := range dbErrors {
				m := e.re.FindStringSubmatch(logs)
				if m == nil {
					continue
				}
				d := ix.about(p)
				d.Code = DBUnreachable
				d.Evidence = []string{strings.TrimSpace(lineContaining(logs, m[0]))}
				switch {
				case e.cause == "auth":
					d.Summary = d.who() + " cannot log in to PostgreSQL: " + m[0]
					d.Suggestion = "It connects with other credentials than the preview's. Use the DATABASE_URL Heimdall " +
						"injects instead of a hard-coded user or password."
				case e.cause == "dns" && m[1] != "postgres":
					d.Summary = fmt.Sprintf("%s connects to %q, which does not exist in the preview", d.who(), m[1])
					d.Suggestion = "Previews have their own PostgreSQL at the address in DATABASE_URL. Read the address " +
						"from DATABASE_URL instead of a hard-coded host."
				case down != "":
					affected = append(affected, d) // reported once, about PostgreSQL (below)
				case haveDB && !dbReady:
					d.Summary = d.who() + " cannot reach PostgreSQL, which is not ready"
					d.Suggestion = "PostgreSQL itself is not ready; its own diagnosis (above) is the cause."
				case e.cause == "starting":
					d.Summary = d.who() + " connected while PostgreSQL was still starting"
					d.Suggestion = "Make " + d.Workload + " retry its database connection at startup instead of exiting."
				case e.cause == "lost":
					d.Summary = d.who() + " lost its connection to PostgreSQL: " + m[0]
					d.Suggestion = "PostgreSQL restarted or went away while " + d.Workload + " was connected. Make it handle a " +
						"dropped connection (reconnect, and handle the pool's error event) instead of crashing."
				default:
					d.Summary = d.who() + " cannot reach PostgreSQL: " + m[0]
					d.Suggestion = "Check that " + d.Workload + " uses DATABASE_URL as injected, and that it retries while " +
						"the database starts."
				}
				if down == "" {
					out = append(out, d)
				}
				break
			}
		}
	}
	if down == "" {
		return out
	}
	var who []string
	for _, a := range affected {
		if !slices.Contains(who, a.Workload) {
			who = append(who, a.Workload)
		}
		db.Evidence = append(db.Evidence, a.Workload+": "+a.Evidence[0])
	}
	slices.Sort(who)
	if len(who) > 0 {
		db.Summary += ": " + strings.Join(who, ", ") + " cannot reach it"
		db.explains = who
	}
	return append(out, db)
}

func lineContaining(text, s string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return s
}

// --- Jobs: migrations, imports, smoke tests ----------------------------------

var (
	// smokeURL finds the URL a smoke test calls: its service is the host.
	smokeURL     = regexp.MustCompile(`https?://([a-z0-9][a-z0-9-]*)(?::\d+)?(/[^\s"']*)?`)
	curlHTTP     = regexp.MustCompile(`curl: \(22\) The requested URL returned error: (\d+)`)
	curlConnect  = regexp.MustCompile(`curl: \(7\) Failed to connect to (\S+) port (\d+)`)
	curlResolve  = regexp.MustCompile(`curl: \(6\) Could not resolve host: (\S+)`)
	curlTimedOut = regexp.MustCompile(`curl: \(28\)`)
)

// curlFailure explains curl's exit in a smoke test's output, given the URL
// it called (service, path), or returns "".
func curlFailure(logs, service, path string) (summary, suggestion string) {
	logsHint := "Read its logs for the request: `heimdall logs --workload " + service + "`."
	if m := curlHTTP.FindStringSubmatch(logs); m != nil {
		summary = "GET " + path + " on " + service + " returned HTTP " + m[1]
		switch {
		case m[1] == "404":
			return summary, "The path does not exist on " + service + ": fix the smoke test's URL, or add the route."
		case strings.HasPrefix(m[1], "4"):
			return summary, service + " refused the request (" + m[1] + "): check what it requires (authentication, headers). " + logsHint
		}
		return summary, service + " failed to handle the request. " + logsHint
	}
	if m := curlConnect.FindStringSubmatch(logs); m != nil {
		return "could not connect to " + m[1] + " port " + m[2],
			"Nothing accepts connections there. Check the service's `port:` and that it binds 0.0.0.0."
	}
	if m := curlResolve.FindStringSubmatch(logs); m != nil {
		return "could not resolve " + m[1],
			"The smoke test calls a host that does not exist in the preview. Use a service name from heimdall.yaml."
	}
	if curlTimedOut.MatchString(logs) {
		return "the request to " + service + " timed out", "The endpoint did not answer in time. " + logsHint
	}
	return "", ""
}

func jobRule(s *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for _, j := range ix.jobs {
		failed := ""
		for _, c := range j.Status.Conditions {
			if (c.Type == batchv1.JobFailed || c.Type == batchv1.JobFailureTarget) && c.Status == corev1.ConditionTrue {
				failed = c.Reason
				if c.Message != "" {
					failed += ": " + c.Message
				}
			}
		}
		if failed == "" {
			continue
		}
		w := workloadOf(j.Spec.Template.Labels)
		d := Diagnosis{Subject: "job/" + j.Name, Workload: w.name, Stage: w.stage, component: w.component}
		var logs string
		var exit int32 = -1
		for _, p := range ix.jobPodsOf(j.Name) {
			for _, cs := range statuses(&p) {
				if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
					exit = t.ExitCode
				}
				l := s.logFor(p.Name, cs.Name, false)
				if len(lastLines(l, 1)) == 0 && cs.State.Terminated != nil {
					l = cs.State.Terminated.Message
				}
				if l != "" {
					logs = l
				}
			}
		}
		deadline := strings.HasPrefix(failed, "DeadlineExceeded")
		d.Evidence = append([]string{"Job " + failed}, withoutProgress(lastLines(logs, 12), 8)...)
		switch w.component {
		case "migration", "seed":
			d.Code, d.Summary = MigrationFailed, "the migration failed"
			if w.component == "seed" {
				d.Code, d.Summary = SeedFailed, "the data import failed"
			}
			if sqlErr := ParseSQLError(logs); sqlErr != nil {
				d.SQL = sqlErr
				d.Summary = strings.TrimPrefix(d.Summary, "the ") + ": " + sqlErr.Summary()
				d.Suggestion = sqlErr.Suggestion()
				d.Evidence = append([]string{"Job " + failed}, sqlEvidence(logs, sqlErr)...)
			} else if deadline {
				d.Summary += " to finish within its deadline"
				d.Suggestion = "It ran out of time: a long data migration, or a lock held by another session. Make it " +
					"faster or split it."
			} else {
				if line := firstError(logs); line != "" {
					d.Summary += ": " + line
				}
				d.Suggestion = "Read its output below; run the same migration against a local PostgreSQL before pushing."
			}
			d.Summary = upperFirst(d.Summary)
		case "smoke-test":
			d.Code = SmokeTestFailed
			name := strings.TrimPrefix(w.name, "heimdall-smoke-")
			cmd := ""
			if cs := j.Spec.Template.Spec.Containers; len(cs) > 0 {
				cmd = strings.Join(append(slices.Clone(cs[0].Command), cs[0].Args...), " ")
				cmd = strings.TrimPrefix(strings.TrimPrefix(cmd, "/bin/sh -c "), "sh -c ")
			}
			d.Summary = fmt.Sprintf("smoke test %q failed", name)
			if exit >= 0 {
				d.Summary += " with exit code " + strconv.Itoa(int(exit))
			}
			d.Suggestion = "The preview started but this check failed. Run the command against the preview, or read " +
				"the service's logs."
			service, path := "the service", "/"
			if m := smokeURL.FindStringSubmatch(cmd); m != nil {
				service, path = m[1], cmp.Or(m[2], "/")
			}
			if summary, suggestion := curlFailure(logs, service, path); summary != "" {
				d.Summary += ": " + summary
				d.Suggestion = suggestion
			}
			if deadline {
				d.Summary += ": it did not finish within its timeout"
			}
			if cmd != "" {
				d.Evidence = append([]string{"command: " + cmd}, d.Evidence...)
			}
			d.Summary = upperFirst(d.Summary)
		default:
			d.Code = Unclassified
			d.Summary = j.Name + " failed"
			if line := firstError(logs); line != "" {
				d.Summary += ": " + line
			}
			d.Suggestion = "Read the Job's output below."
		}
		out = append(out, d)
	}
	return out
}

// sqlEvidence keeps the output lines that carry a SQL error: its message,
// its SQLSTATE, PostgreSQL's DETAIL/HINT/LINE context and the client's
// table/column fields. A client's stack trace says nothing about the SQL.
func sqlEvidence(logs string, e *SQLError) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case e.Message != "" && strings.Contains(l, e.Message),
			strings.Contains(l, e.State) && (strings.Contains(l, "code") || strings.Contains(l, "SQLSTATE") || strings.Contains(l, "State")),
			strings.HasPrefix(t, "DETAIL:"), strings.HasPrefix(t, "HINT:"), strings.HasPrefix(t, "LINE "),
			strings.HasPrefix(t, "table: '"), strings.HasPrefix(t, "column: '"), strings.HasPrefix(t, "constraint: '"):
			out = append(out, l)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// progressLine matches curl's (and similar tools') progress meter.
var progressLine = regexp.MustCompile(`^\s*(% Total|Dload|[0-9:.\skMG%-]+$)`)

// withoutProgress drops progress-meter lines and keeps the last n.
func withoutProgress(lines []string, n int) []string {
	out := slices.DeleteFunc(lines, progressLine.MatchString)
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func (ix *index) jobPodsOf(job string) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range ix.pods {
		if ownerOf(&p, "Job") == job {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b corev1.Pod) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
	return out
}

// --- health checks and endpoints ---------------------------------------------

var (
	probeStatus  = regexp.MustCompile(`statuscode: (\d+)`)
	probeRefused = regexp.MustCompile(`connect: connection refused`)
	probeTimeout = regexp.MustCompile(`(?i)(context deadline exceeded|timeout|Client\.Timeout)`)
)

func healthRule(_ *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for i := range ix.pods {
		p := &ix.pods[i]
		evs := ix.podEvents(p, "Unhealthy")
		if len(evs) == 0 {
			continue
		}
		ready := slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
			return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		})
		if ready {
			evs = slices.DeleteFunc(evs, func(e corev1.Event) bool {
				if !strings.HasPrefix(e.Message, "Liveness") || len(p.Status.ContainerStatuses) == 0 {
					return false
				}
				for _, cs := range p.Status.ContainerStatuses {
					if !cs.Ready || cs.State.Running == nil || !eventTime(e).Before(cs.State.Running.StartedAt.Time) {
						return false
					}
				}
				return true // every current container recovered after this warning
			})
			if len(evs) == 0 {
				continue
			}
		}
		liveness := slices.ContainsFunc(evs, func(e corev1.Event) bool { return strings.HasPrefix(e.Message, "Liveness") })
		if ready && !liveness {
			continue // recovered
		}
		d := ix.about(p)
		d.Code = HealthcheckFailed
		probe, path, port := "readiness", "", ""
		if liveness {
			probe = "liveness"
		}
		for _, c := range p.Spec.Containers {
			pr := c.ReadinessProbe
			if liveness {
				pr = c.LivenessProbe
			}
			if pr != nil && pr.HTTPGet != nil {
				path, port = pr.HTTPGet.Path, portNumber(c, pr.HTTPGet.Port.String())
			} else if pr != nil && pr.TCPSocket != nil {
				port = portNumber(c, pr.TCPSocket.Port.String())
			}
		}
		target := "port " + port
		if path != "" {
			target = "GET " + path + " on port " + port
		}
		msg := evs[len(evs)-1].Message
		switch {
		case probeStatus.MatchString(msg):
			code := probeStatus.FindStringSubmatch(msg)[1]
			d.Summary = fmt.Sprintf("%s fails its %s check: %s returns HTTP %s", d.who(), probe, target, code)
			if strings.HasPrefix(code, "4") {
				d.Suggestion = fmt.Sprintf("`health.path` is %s, but the app answers %s there. Point `health.path` at an "+
					"endpoint that returns 2xx, or add one.", path, code)
			} else {
				d.Suggestion = "The app is up but reports itself unhealthy. Its logs say why; often a dependency it checks."
			}
		case probeRefused.MatchString(msg):
			d.Summary = fmt.Sprintf("%s fails its %s check: nothing listens on port %s", d.who(), probe, port)
			d.Suggestion = "Make `port:` in heimdall.yaml match the port the app listens on, and make it bind 0.0.0.0 " +
				"rather than 127.0.0.1."
		case probeTimeout.MatchString(msg):
			d.Summary = fmt.Sprintf("%s fails its %s check: %s does not answer in time", d.who(), probe, target)
			d.Suggestion = "The health endpoint is too slow (the probe waits 1-5s). Make it cheap, and check what it waits on."
		default:
			d.Summary = fmt.Sprintf("%s fails its %s check: %s", d.who(), probe, firstLine(msg))
			d.Suggestion = "Check the health endpoint and the app's logs."
		}
		if liveness {
			d.Suggestion += " Failing liveness checks make Kubernetes restart it."
		}
		d.Evidence = eventLines(evs)
		out = append(out, d)
	}
	return out
}

// portNumber resolves a named container port ("http") to its number.
func portNumber(c corev1.Container, port string) string {
	for _, p := range c.Ports {
		if p.Name == port {
			return strconv.Itoa(int(p.ContainerPort))
		}
	}
	return port
}

func endpointsRule(s *Snapshot, ix *index) []Diagnosis {
	var out []Diagnosis
	for _, svc := range s.Services {
		if svc.DeletionTimestamp != nil || !s.current(svc.Labels) || len(svc.Spec.Selector) == 0 || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		readyCount := 0
		for _, es := range s.EndpointSlices {
			if es.DeletionTimestamp != nil || !s.current(es.Labels) || es.Labels["kubernetes.io/service-name"] != svc.Name {
				continue
			}
			for _, ep := range es.Endpoints {
				if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
					continue
				}
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					readyCount++
				}
			}
		}
		if readyCount > 0 {
			continue
		}
		matching := 0
		for _, p := range ix.pods {
			if labelsMatch(svc.Spec.Selector, p.Labels) {
				matching++
			}
		}
		w := workloadOf(svc.Labels)
		d := Diagnosis{Code: NoEndpoints, Subject: "service/" + svc.Name, Workload: svc.Name, Stage: w.stage, component: w.component,
			Severity: SeverityWarning}
		if matching == 0 {
			d.Summary = "service " + svc.Name + " has no pods: none match its selector"
			d.Suggestion = "Nothing backs the service. Its workload may have failed to create pods (see other findings)."
		} else {
			d.Summary = fmt.Sprintf("service %s has no ready endpoints: none of its %d pods is ready", svc.Name, matching)
			d.Suggestion = "Requests to it fail until a pod is ready. Check the workload's health check and logs."
		}
		out = append(out, d)
	}
	return out
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// who names a diagnosis's subject at the start of a sentence: app workloads
// by their name in heimdall.yaml, Heimdall's own Jobs by what they do.
func (d Diagnosis) who() string {
	switch d.component {
	case "migration":
		return "The migration Job"
	case "seed":
		return "The data import Job"
	case "database-admin":
		return "The database preparation Job"
	case "smoke-test":
		return "Smoke test " + strconv.Quote(strings.TrimPrefix(d.Workload, "heimdall-smoke-"))
	}
	return d.Workload
}
