package diagnose

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// Options control Collect.
type Options struct {
	Namespace string
	// Generation the environment is being deployed at; 0 reads it from the
	// engine's journal.
	Generation int64
	// Failure, when the caller knows it; nil reads the last failed step from
	// the engine's journal.
	Failure        *Failure
	ConfigProblems []ConfigProblem
	// TailLines and MaxLogBytes cap each container's log; MaxLogs caps how
	// many containers' logs are read. Defaults: 60 lines, 16 KiB, 20.
	TailLines   int
	MaxLogBytes int
	MaxLogs     int
	// MaxEvents caps events, preferring warnings about current workloads,
	// then the most recent events. Default 300.
	MaxEvents int
	// Dynamic, when set, also reads the preview's HTTPRoutes (Gateway API),
	// so a route the Gateway rejected is diagnosed. Clusters without the
	// Gateway API are skipped silently.
	Dynamic dynamic.Interface
	Now     func() time.Time
}

var httpRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

// journalName and journalRecord mirror the engine's operation journal
// (internal/engine/journal.go); diagnose cannot import the engine.
const journalName = "heimdall-operation"

type journalRecord struct {
	Generation int64 `json:"generation"`
	Events     []struct {
		Stage      string `json:"stage"`
		State      string `json:"state"`
		Code       string `json:"code"`
		Generation int64  `json:"generation"`
	} `json:"events"`
}

// Collect captures a snapshot of one preview namespace. Log lines and
// messages are redacted with the exact values injected into the preview
// (read from its credentials and secrets), then patterns; if those values
// cannot be read, no logs are collected at all.
func Collect(ctx context.Context, c kubernetes.Interface, o Options) (*Snapshot, error) {
	if o.TailLines < 0 || o.MaxLogBytes < 0 || o.MaxLogs < 0 || o.MaxEvents < 0 {
		return nil, fmt.Errorf("diagnosis limits must not be negative")
	}
	o.TailLines = cmp.Or(o.TailLines, 60)
	o.MaxLogBytes = cmp.Or(o.MaxLogBytes, 16<<10)
	o.MaxLogs = cmp.Or(o.MaxLogs, 20)
	o.MaxEvents = cmp.Or(o.MaxEvents, 300)
	if o.Now == nil {
		o.Now = time.Now
	}
	ns := o.Namespace
	s := &Snapshot{Version: SnapshotVersion, CapturedAt: o.Now().UTC().Truncate(time.Second), Namespace: ns,
		Generation: o.Generation, Failure: o.Failure, ConfigProblems: o.ConfigProblems}
	if _, err := c.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("preview namespace %s: %w", ns, err)
	}
	if err := readJournal(ctx, c, s); err != nil {
		return nil, err
	}
	list := metav1.ListOptions{}
	var err error
	collect := func(f func() error) {
		if err == nil {
			err = f()
		}
	}
	collect(func() error {
		l, e := c.CoreV1().Pods(ns).List(ctx, list)
		if e == nil {
			s.Pods = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.CoreV1().Events(ns).List(ctx, list)
		if e == nil {
			s.Events = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.BatchV1().Jobs(ns).List(ctx, list)
		if e == nil {
			s.Jobs = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.AppsV1().Deployments(ns).List(ctx, list)
		if e == nil {
			s.Deployments = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.AppsV1().StatefulSets(ns).List(ctx, list)
		if e == nil {
			s.StatefulSets = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.CoreV1().Services(ns).List(ctx, list)
		if e == nil {
			s.Services = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.DiscoveryV1().EndpointSlices(ns).List(ctx, list)
		if e == nil {
			s.EndpointSlices = l.Items
		}
		return e
	})
	collect(func() error {
		l, e := c.CoreV1().ResourceQuotas(ns).List(ctx, list)
		if e == nil {
			s.Quotas = l.Items
		}
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("read preview %s: %w", ns, err)
	}
	if o.Dynamic != nil {
		routes, err := o.Dynamic.Resource(httpRoutes).Namespace(ns).List(ctx, list)
		switch {
		case err == nil:
			for i := range routes.Items {
				var r gatewayv1.HTTPRoute
				if runtime.DefaultUnstructuredConverter.FromUnstructured(routes.Items[i].Object, &r) == nil {
					s.Routes = append(s.Routes, r)
				}
			}
		case apierrors.IsNotFound(err):
			// No Gateway API in this cluster: nothing routes to the preview.
		default:
			s.Notes = append(s.Notes, "routes not inspected: "+string(apierrors.ReasonForError(err)))
		}
	}
	if len(s.Events) > o.MaxEvents {
		// Routine events and failures of deleted/old-generation objects must
		// not evict the warnings the rules need to explain this operation.
		relevant := map[string]bool{}
		for _, events := range newIndex(s).events {
			for _, e := range events {
				relevant[e.Name] = true
			}
		}
		slices.SortFunc(s.Events, func(a, b corev1.Event) int {
			priority := func(e corev1.Event) int {
				if relevant[e.Name] {
					return 1
				}
				return 0
			}
			return cmp.Or(cmp.Compare(priority(a), priority(b)), eventTime(a).Compare(eventTime(b)), cmp.Compare(a.Name, b.Name))
		})
		s.Events = s.Events[len(s.Events)-o.MaxEvents:]
	}
	slices.SortFunc(s.Events, func(a, b corev1.Event) int {
		return cmp.Or(eventTime(a).Compare(eventTime(b)), cmp.Compare(a.Name, b.Name))
	})

	var specs []corev1.PodSpec
	for _, p := range newIndex(s).pods {
		specs = append(specs, p.Spec)
	}
	secrets, secretErr := SecretValues(ctx, c.CoreV1(), ns, specs...)
	r := redact.New(secrets...)
	if secretErr != nil {
		if status, ok := secretErr.(apierrors.APIStatus); ok && apierrors.IsNotFound(secretErr) {
			if details := status.Status().Details; details != nil && details.Kind == "secrets" && details.Name != "" {
				s.MissingSecrets = []string{details.Name}
			}
		}
		s.Notes = append(s.Notes, "logs omitted: the preview's secrets could not be read to redact them ("+
			string(apierrors.ReasonForError(secretErr))+")")
	} else {
		s.Logs = readLogs(ctx, c, s, o, r)
	}
	s.Sanitize(r)
	if secretErr != nil {
		// Unknown injected values may also appear in events, termination
		// messages or arguments. Fail closed for all free-form evidence.
		s.OmitSensitiveText()
	}
	return s, nil
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// readJournal fills the generation and the last failed step from the
// engine's journal, unless the caller supplied them.
func readJournal(ctx context.Context, c kubernetes.Interface, s *Snapshot) error {
	cm, err := c.CoreV1().ConfigMaps(s.Namespace).Get(ctx, journalName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the engine's journal: %w", err)
	}
	var rec journalRecord
	if err := json.Unmarshal([]byte(cm.Data["record"]), &rec); err != nil {
		return nil // an unreadable journal only loses context
	}
	s.AcceptedGeneration = rec.Generation
	if s.Generation == 0 {
		s.Generation = rec.Generation
	}
	for i := len(rec.Events) - 1; i >= 0; i-- {
		ev := rec.Events[i]
		generation := ev.Generation
		if generation == 0 { // journals written before events carried generation
			generation = rec.Generation
		}
		if s.Generation != generation {
			continue
		}
		// The journal retains history across retries and generations. Only
		// the latest event can describe the current failure: a subsequent
		// running/succeeded/ready event means the old failure was superseded.
		if ev.State == "failed" {
			switch {
			case s.Failure == nil:
				s.Failure = &Failure{Code: ev.Code, Step: ev.Stage}
			case s.Failure.Step == "" && s.Failure.Code == ev.Code:
				s.Failure.Step = ev.Stage
			}
		}
		break
	}
	return nil
}

// SecretValues returns the preview's generated credentials, tenant secrets
// and any secrets referenced by its pods. Referenced secrets must be readable,
// even when marked optional: an existing container may still hold the value
// of a secret that was removed after it started.
func SecretValues(ctx context.Context, c typedcore.CoreV1Interface, ns string, specs ...corev1.PodSpec) ([]string, error) {
	required := map[string]bool{render.CredentialsSecret: false, render.AppSecretsSecret: false}
	for _, spec := range specs {
		containers := append(slices.Clone(spec.InitContainers), spec.Containers...)
		for _, ec := range spec.EphemeralContainers {
			containers = append(containers, corev1.Container{Env: ec.Env, EnvFrom: ec.EnvFrom})
		}
		for _, ct := range containers {
			for _, env := range ct.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					required[env.ValueFrom.SecretKeyRef.Name] = true
				}
			}
			for _, from := range ct.EnvFrom {
				if from.SecretRef != nil {
					required[from.SecretRef.Name] = true
				}
			}
		}
		for _, volume := range spec.Volumes {
			if volume.Secret != nil {
				required[volume.Secret.SecretName] = true
			}
			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil {
						required[source.Secret.Name] = true
					}
				}
			}
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		if name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var values []string
	for _, name := range names {
		s, err := c.Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !required[name] {
			continue
		}
		if err != nil {
			return values, err
		}
		for _, b := range s.Data {
			values = append(values, string(b))
		}
	}
	return values, nil
}

// readLogs fetches the tails that matter: a crashed container's previous
// instance, failed Job pods, containers that are running but not ready.
func readLogs(ctx context.Context, c kubernetes.Interface, s *Snapshot, o Options, r *redact.Redactor) []Log {
	type want struct {
		pod, container string
		previous       bool
		fromStart      bool
		priority       int
	}
	var wants []want
	// Use the same generation/Job filtering as the rules before allocating
	// the log budget, so old failed pods cannot crowd out the current error.
	for _, p := range newIndex(s).pods {
		job := ownerOf(&p, "Job") != ""
		for _, cs := range statuses(&p) {
			switch {
			case cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0:
				wants = append(wants, want{p.Name, cs.Name, true, true, 1})
				// Restarted and still unready: what it prints now matters too.
				if cs.State.Running != nil && !cs.Ready {
					wants = append(wants, want{p.Name, cs.Name, false, false, 2})
				}
			case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
				wants = append(wants, want{p.Name, cs.Name, false, true, 0})
			case job && cs.State.Terminated != nil:
				continue // a successful Job step
			case cs.State.Running != nil && !cs.Ready:
				wants = append(wants, want{p.Name, cs.Name, false, false, 2})
			}
		}
	}
	slices.SortStableFunc(wants, func(a, b want) int {
		return cmp.Or(cmp.Compare(a.priority, b.priority), cmp.Compare(a.pod, b.pod), cmp.Compare(a.container, b.container))
	})
	if len(wants) > o.MaxLogs {
		wants = wants[:o.MaxLogs]
	}
	// A crashed instance is read from its start: its whole output usually
	// fits, and the error a crash prints comes before the dump (a client
	// object, a stack) that would push it out of any tail. Only when it does
	// not fit is its end read as well. Running instances are read from the
	// end.
	headLimit := int64(o.MaxLogBytes * 4)
	tailLines, tailLimit := int64(o.TailLines*4), int64(o.MaxLogBytes*4)
	stream := func(pod, container string, previous, fromStart bool) (string, error) {
		opts := &corev1.PodLogOptions{Container: container, Previous: previous, LimitBytes: &headLimit}
		if !fromStart {
			opts.TailLines, opts.LimitBytes = &tailLines, &tailLimit
		}
		rc, err := c.CoreV1().Pods(s.Namespace).GetLogs(pod, opts).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.LimitReader(rc, max(headLimit, tailLimit)))
		return buf.String(), nil
	}
	read := func(pod, container string, previous, fromStart bool) (string, error) {
		if !fromStart {
			return stream(pod, container, previous, false)
		}
		head, err := stream(pod, container, previous, true)
		if err != nil || int64(len(head)) < headLimit {
			return head, err
		}
		end, err := stream(pod, container, previous, false)
		if err != nil {
			return head, nil
		}
		return strings.Join(errorLines(head, 5), "\n") + "\n[... output omitted]\n" + end, nil
	}
	var logs []Log
	current := map[[2]string]bool{} // containers whose current log was read
	for _, w := range wants {
		key := [2]string{w.pod, w.container}
		if !w.previous && current[key] {
			continue
		}
		text, err := read(w.pod, w.container, w.previous, w.fromStart)
		// The kubelet keeps one previous instance; when it is gone it answers
		// with a notice instead. The current instance is the next best thing.
		if err == nil && w.previous && strings.HasPrefix(text, "unable to retrieve container logs") && !current[key] {
			w.previous = false
			text, err = read(w.pod, w.container, false, false)
		}
		if !w.previous {
			current[key] = true
		}
		if err != nil {
			text = "[logs unavailable: " + string(apierrors.ReasonForError(err)) + "]"
		} else {
			text = condense(r.Lines(text, 0, 0), o.TailLines, o.MaxLogBytes)
		}
		logs = append(logs, Log{Pod: w.pod, Container: w.container, Previous: w.previous, Text: strings.TrimRight(text, "\n")})
	}
	return logs
}

// condense fits a (redacted) log into maxLines and maxBytes: the first few
// lines that look like errors, then as much of the end as fits, marking what
// was left out.
func condense(text string, maxLines, maxBytes int) string {
	if maxLines <= 0 || maxBytes <= 0 {
		return ""
	}
	text = strings.TrimRight(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines && len(text) <= maxBytes {
		return text
	}
	clip := func(l string, limit int) string {
		if len(l) > limit {
			// Byte limits must not cut a multi-byte character in half.
			end := limit
			for end > 0 && !utf8.RuneStart(l[end]) {
				end--
			}
			return l[:end]
		}
		return l
	}
	// A tiny caller-specified budget cannot fit a head, marker and tail.
	// Keep the first error itself (or the last line if no error is present).
	if maxLines == 1 || maxBytes <= 64 {
		for _, l := range lines {
			if isErrorLine(l) {
				return clip(l, maxBytes)
			}
		}
		return clip(lines[len(lines)-1], maxBytes)
	}
	const maxHead = 5
	var head []string
	headBytes, lastHead := 0, -1
	for i, l := range lines {
		if len(head) == min(maxHead, maxLines-1) {
			break
		}
		if isErrorLine(l) {
			available := maxBytes - headBytes - 64
			if available <= 0 {
				break
			}
			l = clip(l, min(maxEvidenceWidth*2, available))
			if l == "" {
				break
			}
			head = append(head, l)
			headBytes += len(head[len(head)-1]) + 1
			lastHead = i
		}
	}
	// The end, within what is left of both budgets, after the head.
	var tail []string
	budget := maxBytes - headBytes - 64
	for i := len(lines) - 1; i > lastHead && len(tail) < maxLines-len(head)-1; i-- {
		l := clip(lines[i], maxEvidenceWidth*2)
		if budget -= len(l) + 1; budget < 0 {
			break
		}
		tail = append([]string{l}, tail...)
	}
	omitted := len(lines) - len(head) - len(tail)
	out := head
	if omitted > 0 {
		out = append(out, "[... "+strconv.Itoa(omitted)+" lines omitted]")
	}
	return strings.Join(append(out, tail...), "\n")
}

// errorHead matches a line that states an error ("Error: ...", "TypeError:
// ...", "error: column ...", "FATAL: ...", "panic: ...", "Traceback"), not one
// that merely mentions one ("error: [Function: ...]" in an object dump).
var errorHead = regexp.MustCompile(`(?i)^\s*(?:uncaught\s+)?(?:[a-z]*error|fatal|panic|traceback|exception)\b\s*[:(]\s*\S`)

func isErrorLine(l string) bool {
	t := strings.TrimSpace(l)
	if !errorHead.MatchString(l) || strings.HasPrefix(t, "at ") {
		return false
	}
	return !isErrorDumpLine(t)
}

func isErrorDumpLine(l string) bool {
	t := strings.TrimSpace(l)
	for _, dump := range []string{"[Function", "undefined", "null,", "{", "[]", "[Object"} {
		if strings.HasSuffix(t, dump) || strings.Contains(t, ": "+dump) {
			return true
		}
	}
	return false
}

// errorLines returns the first n error lines of text.
func errorLines(text string, n int) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if len(out) < n && isErrorLine(l) {
			out = append(out, l)
		}
	}
	return out
}

// ForFailure is the snapshot of a failure that could not be inspected in the
// cluster (it happened before the namespace existed, or the namespace cannot
// be read): the failure alone still gets its diagnosis.
func ForFailure(namespace string, generation int64, f *Failure, why error) *Snapshot {
	s := &Snapshot{Version: SnapshotVersion, Namespace: namespace, Generation: generation, Failure: f}
	if why != nil {
		s.Notes = []string{"the preview could not be inspected: " + why.Error()}
	}
	s.Sanitize(redact.New())
	return s
}
