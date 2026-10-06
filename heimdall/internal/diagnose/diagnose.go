package diagnose

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Diagnosis is one finding.
type Diagnosis struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	// Summary says specifically what is wrong, in one line.
	Summary string `json:"summary"`
	// Subject is the object at fault: "deployment/api", "job/heimdall-migrate-g2".
	Subject  string `json:"subject,omitempty"`
	Workload string `json:"workload,omitempty"`
	Stage    string `json:"stage,omitempty"`
	// Suggestion says what to do.
	Suggestion string `json:"suggestion"`
	// Evidence is a few redacted lines that support the summary.
	Evidence []string `json:"evidence,omitempty"`
	// SQL is the parsed PostgreSQL error of a failed migration or import.
	SQL *SQLError `json:"sql,omitempty"`

	component string
	// explains lists other workloads whose symptoms this finding causes
	// (the apps that cannot reach a stopped database).
	explains []string
}

// Report is the ranked diagnosis of one snapshot.
type Report struct {
	Namespace  string `json:"namespace"`
	Generation int64  `json:"generation"`
	// Diagnoses, most likely root cause first.
	Diagnoses []Diagnosis `json:"diagnoses"`
}

// RootCause is the top-ranked diagnosis, or nil.
func (r *Report) RootCause() *Diagnosis {
	if r == nil || len(r.Diagnoses) == 0 {
		return nil
	}
	return &r.Diagnoses[0]
}

// Limits on what a diagnosis carries.
const (
	maxEvidenceLines = 10
	maxEvidenceWidth = 240
)

type rule func(*Snapshot, *index) []Diagnosis

var rules = []rule{configRule, imagePullRule, schedulingRule, quotaRule, containerRule, databaseRule, jobRule,
	healthRule, endpointsRule, routeRule}

// Diagnose explains a snapshot. It is pure and deterministic: the same
// snapshot always yields the same report.
func Diagnose(s *Snapshot) *Report {
	ix := newIndex(s)
	var ds []Diagnosis
	for _, r := range rules {
		ds = append(ds, r(s, ix)...)
	}
	ds = suppressSymptoms(merge(ds))
	lostOwnership := s.Failure != nil && s.Failure.Code == "engine.lock_lost"
	if lostOwnership {
		// Workload failures cannot explain losing journal ownership. Keep
		// that operation failure first, with independent findings secondary.
		ds = append([]Diagnosis{unclassified(s, ix)}, ds...)
	} else if s.Failure != nil && !slices.ContainsFunc(ds, func(d Diagnosis) bool {
		return d.Severity != SeverityWarning || d.Code == StaleGeneration
	}) {
		ds = append(ds, unclassified(s, ix))
	}
	for i := range ds {
		d := &ds[i]
		d.Title = d.Code.Title()
		if d.Severity == "" {
			d.Severity = SeverityError
		}
		d.Evidence = tidy(d.Evidence)
	}
	if lostOwnership {
		rank(ds[1:])
	} else {
		rank(ds)
	}
	if ds == nil {
		ds = []Diagnosis{}
	}
	return &Report{Namespace: s.Namespace, Generation: s.Generation, Diagnoses: ds}
}

// merge combines findings of the same code about the same subject.
func merge(ds []Diagnosis) []Diagnosis {
	var out []Diagnosis
	seen := map[string]int{}
	for _, d := range ds {
		key := string(d.Code) + "\x00" + d.Subject
		if i, ok := seen[key]; ok {
			out[i].Evidence = append(out[i].Evidence, d.Evidence...)
			continue
		}
		seen[key] = len(out)
		out = append(out, d)
	}
	return out
}

// causes explain a workload's failing health checks and missing endpoints.
var causes = map[Code]bool{ConfigInvalid: true, PolicyDenied: true, QuotaExceeded: true, NoCapacity: true,
	ImagePullFailed: true, DBUnreachable: true, OutOfMemory: true, ContainerCrash: true}

// suppressSymptoms drops findings that only restate another finding about
// the same workload: no endpoints or failing readiness because its pods
// crash, and so on.
func suppressSymptoms(ds []Diagnosis) []Diagnosis {
	explained, unhealthy := map[string]bool{}, map[string]bool{}
	for _, d := range ds {
		if d.Workload == "" {
			continue
		}
		explained[d.Workload] = explained[d.Workload] || causes[d.Code]
		unhealthy[d.Workload] = unhealthy[d.Workload] || d.Code == HealthcheckFailed
		for _, w := range d.explains {
			explained[w] = true
		}
	}
	return slices.DeleteFunc(ds, func(d Diagnosis) bool {
		switch d.Code {
		case HealthcheckFailed:
			return explained[d.Workload]
		case NoEndpoints:
			// A Service without ready pods restates its pods' problem; a route
			// the Gateway rejected is a problem of its own.
			return strings.HasPrefix(d.Subject, "service/") && (explained[d.Workload] || unhealthy[d.Workload])
		}
		return false
	})
}

// rank orders root causes first: an earlier stage before a later one (a
// failed migration before an API that is not ready), then the code's tier
// (what prevents starting before what fails at runtime), then the preview's
// own dependencies before what depends on them.
func rank(ds []Diagnosis) {
	depth := func(d Diagnosis) int {
		switch {
		case dependencyComponents[d.component]:
			return 0
		case d.component == "database-admin" || d.component == "migration" || d.component == "seed":
			return 1
		case d.component == "smoke-test":
			return 3
		}
		return 2
	}
	slices.SortStableFunc(ds, func(a, b Diagnosis) int {
		return cmp.Or(
			cmp.Compare(stageIndex(a.Stage), stageIndex(b.Stage)),
			cmp.Compare(catalog[a.Code].tier, catalog[b.Code].tier),
			cmp.Compare(depth(a), depth(b)),
			cmp.Compare(order[a.Code], order[b.Code]),
			cmp.Compare(a.Subject, b.Subject),
		)
	})
}

// tidy deduplicates evidence, trims long lines and caps the count.
func tidy(lines []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" || seen[l] {
			continue
		}
		seen[l] = true
		if r := []rune(l); len(r) > maxEvidenceWidth {
			l = string(r[:maxEvidenceWidth-1]) + "…"
		}
		out = append(out, l)
	}
	if len(out) > maxEvidenceLines {
		out = out[len(out)-maxEvidenceLines:]
	}
	return out
}

func unclassified(s *Snapshot, ix *index) Diagnosis {
	f := s.Failure
	d := Diagnosis{Code: Unclassified, Summary: f.Code, Stage: stageOfStep(f.Step), Subject: f.Step,
		Suggestion: "No rule explains this failure yet. Check `heimdall status` and `heimdall logs` for the step above, " +
			"and report it so a rule can be added."}
	if f.Message != "" {
		d.Summary = f.Code + ": " + f.Message
	}
	if f.Code == "engine.lock_lost" {
		d.Suggestion = "Operation journal ownership could no longer be verified. Check Kubernetes API connectivity " +
			"and the active operation with `heimdall status`; retry after the active operation finishes or its lease expires."
	}
	if f.Code == "engine.timeout" {
		d.Summary = "step " + f.Step + " did not finish within the step timeout"
		d.Suggestion = "Nothing failed outright, but the step never became ready. Check the step's workloads with " +
			"`heimdall status`; if it is just slow, raise the step timeout (`--timeout`, operations.stepTimeout)."
		pending := pendingIn(s, ix, d.Stage)
		if len(pending) > 0 {
			d.Summary += ": " + pending[0]
			d.Evidence = pending
			d.Subject, _, _ = strings.Cut(pending[0], " ")
		}
		if strings.HasPrefix(d.Subject, "job/heimdall-migrate") || strings.HasPrefix(d.Subject, "job/heimdall-seed") {
			d.Suggestion = "It was still running, not failing: a long data migration or import, or one waiting on a " +
				"lock or for input. Make it faster or non-interactive, or raise the step timeout (`--timeout`, " +
				"operations.stepTimeout)."
		}
	}
	return d
}

// pendingIn lists what in a stage was still unfinished: Jobs running and
// workloads without all replicas ready.
func pendingIn(s *Snapshot, ix *index, stage string) []string {
	var out []string
	for _, j := range ix.jobs {
		w := workloadOf(j.Spec.Template.Labels)
		if w.stage == stage && j.Status.Active > 0 {
			out = append(out, "job/"+j.Name+" was still running")
		}
	}
	for _, d := range ix.deployments {
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if workloadOf(d.Labels).stage == stage && d.Status.ReadyReplicas < want {
			out = append(out, fmt.Sprintf("deployment/%s had %d of %d replicas ready", d.Name, d.Status.ReadyReplicas, want))
		}
	}
	for _, st := range ix.statefulSets {
		want := int32(1)
		if st.Spec.Replicas != nil {
			want = *st.Spec.Replicas
		}
		if workloadOf(st.Labels).stage == stage && st.Status.ReadyReplicas < want {
			out = append(out, fmt.Sprintf("statefulset/%s had %d of %d replicas ready", st.Name, st.Status.ReadyReplicas, want))
		}
	}
	slices.Sort(out)
	return out
}

// stageOfStep maps an engine step ("baseline-db/migrate") to its stage.
func stageOfStep(step string) string {
	head, _, _ := strings.Cut(step, "/")
	if stageIndex(head) >= 0 {
		return head
	}
	switch head {
	case "database", "seed", "clone":
		return stages[2]
	case "resume":
		return stages[3]
	}
	return ""
}
