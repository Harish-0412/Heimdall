package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/controlclient"
	"github.com/heimdall-dev/heimdall/internal/controller"
	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/source"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Reporter sends observations and small requested log tails over the outbound
// connection. It never uploads continuous logs or fixture bytes.
type Reporter struct {
	Control    *controlclient.Client
	Source     *source.API
	Reader     client.Reader
	Namespace  string
	Specs      controller.SpecBuilder
	Engine     *engine.Engine
	Kubernetes kubernetes.Interface
	Log        logr.Logger
	last       map[string]string
	versions   map[string]int64
}

func (r *Reporter) NeedLeaderElection() bool { return true }
func (r *Reporter) Start(ctx context.Context) error {
	r.last, r.versions = map[string]string{}, map[string]int64{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	_ = r.Control.Heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			if err := r.Control.Heartbeat(ctx); err != nil {
				r.Log.Error(err, "heartbeat unavailable")
			}
		case <-ticker.C:
			if err := r.Report(ctx); err != nil {
				r.Log.Error(err, "runtime report incomplete")
			}
		}
	}
}
func (r *Reporter) Report(ctx context.Context) error {
	if r.last == nil {
		r.last, r.versions = map[string]string{}, map[string]int64{}
	}
	var list v1alpha1.PreviewEnvironmentList
	if err := r.Reader.List(ctx, &list, client.InNamespace(r.Namespace), client.MatchingLabels{source.LabelSource: "api"}); err != nil {
		return err
	}
	records := r.Source.Records()
	var errs []error
	projected := map[string]bool{}
	for _, pe := range list.Items {
		e, ok := records[pe.Spec.EnvironmentID]
		if ok && pe.Name == e.Name && pe.Spec.Generation == e.Generation {
			projected[e.Id] = true
		}
	}
	for id, failure := range r.Source.PreparationFailures() {
		e, ok := records[id]
		if !ok || e.Generation != failure.Generation || projected[id] {
			continue
		}
		st := v1alpha1.PreviewEnvironmentStatus{Phase: v1alpha1.PhaseFailed,
			LastError: &v1alpha1.ErrorStatus{Code: "source.bundle_unavailable", Message: "The approved configuration bundle could not be prepared in the cluster", Step: "configuration", Generation: failure.Generation, Retryable: true, At: failure.At},
			Diagnoses: []v1alpha1.DiagnosisStatus{{Code: "source.bundle_unavailable", Summary: "The approved configuration bundle is unavailable or failed validation", Stage: "configuration", Suggestion: "Check the agent's registry access, tenant registry policy, bundle digest and sanitised-data attestation; the agent will retry automatically"}}}
		if err := r.reportStatus(ctx, e, st); err != nil {
			errs = append(errs, err)
		}
	}
	for _, pe := range list.Items {
		e, ok := records[pe.Spec.EnvironmentID]
		if !ok || pe.Name != e.Name || pe.Spec.Generation != e.Generation || pe.Status.Phase == "" || pe.Status.ObservedGeneration != pe.Generation {
			continue
		}
		if err := r.reportStatus(ctx, e, pe.Status); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.logs(ctx, records); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Reporter) reportStatus(ctx context.Context, e gen.Environment, status v1alpha1.PreviewEnvironmentStatus) error {
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	h := sha256.Sum256(append([]byte(fmt.Sprintf("%d:", e.Generation)), b...))
	id := hex.EncodeToString(h[:])
	if r.last[e.Id] == id {
		return nil
	}
	var metadata struct {
		TraceParent string `json:"traceParent"`
	}
	if json.Unmarshal(e.Spec, &metadata) == nil {
		ctx = tracecontext.Extract(ctx, metadata.TraceParent)
	}
	ctx, span := otel.Tracer("heimdall.agent").Start(ctx, "preview.report_status")
	defer span.End()
	span.SetAttributes(attribute.String("env_id", e.Id), attribute.Int64("generation", e.Generation))
	version := max(e.Version, r.versions[e.Id])
	in := gen.StatusUpdate{EventID: id, Generation: e.Generation, Version: version, Phase: gen.StatusUpdatePhase(status.Phase), Status: b}
	var result gen.Environment
	if err = r.Control.Do(ctx, "POST", "/v1/agent/environments/"+e.Id+"/status", in, &result); err != nil {
		var he *controlclient.HTTPError
		if errors.As(err, &he) && he.Status == 409 {
			_ = r.Control.Do(ctx, "GET", "/v1/environments/"+e.Id, nil, &result)
			if result.Id == e.Id {
				r.versions[e.Id] = result.Version
			}
		}
		return err
	}
	r.last[e.Id], r.versions[e.Id] = id, result.Version
	return nil
}

func (r *Reporter) logs(ctx context.Context, records map[string]gen.Environment) error {
	var requests []gen.LogRequest
	if err := r.Control.Do(ctx, "GET", "/v1/agent/log-requests", nil, &requests); err != nil {
		return err
	}
	for _, req := range requests {
		if !req.ExpiresAt.After(time.Now()) {
			continue
		}
		result := gen.LogResult{Generation: req.Generation, Redacted: true}
		e, ok := records[req.EnvironmentID]
		text, err := "", errors.New("requested environment is unavailable or stale")
		if ok && e.Generation == req.Generation {
			var pe v1alpha1.PreviewEnvironment
			if r.Reader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: e.Name}, &pe) == nil && pe.Spec.EnvironmentID == e.Id && pe.Labels[source.LabelSource] == "api" && pe.Spec.Generation == req.Generation {
				text, err = r.tail(ctx, &pe, req.Workload, int64(req.Tail))
			}
		}
		if err != nil {
			safe := "Unable to read a scoped, redacted log tail"
			result.Error = &safe
		} else {
			result.Text = &text
		}
		if err = r.Control.Do(ctx, "POST", "/v1/agent/log-requests/"+req.Id, result, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reporter) tail(ctx context.Context, pe *v1alpha1.PreviewEnvironment, workload string, tail int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	spec, _, err := r.Specs.Build(ctx, pe)
	if err != nil {
		return "", err
	}
	pods, err := r.Engine.LogPods(ctx, spec, workload)
	if err != nil || len(pods) == 0 {
		return "", errors.New("no scoped application pods")
	}
	if len(pods) > 4 {
		pods = pods[:4]
	}
	var live []corev1.Pod
	var podSpecs []corev1.PodSpec
	for _, p := range pods {
		pod, err := r.Kubernetes.CoreV1().Pods(p.GetNamespace()).Get(ctx, p.GetName(), metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		live = append(live, *pod)
		podSpecs = append(podSpecs, pod.Spec)
	}
	values, err := diagnose.SecretValues(ctx, r.Kubernetes.CoreV1(), live[0].Namespace, podSpecs...)
	if err != nil {
		return "", errors.New("log redaction secrets unavailable")
	}
	var out strings.Builder
	for _, pod := range live {
		for _, ct := range pod.Spec.Containers {
			limit := int64(32 << 10)
			stream, err := r.Kubernetes.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: ct.Name, TailLines: &tail, LimitBytes: &limit}).Stream(ctx)
			if err != nil {
				return "", errors.New("log stream unavailable")
			}
			b, err := io.ReadAll(io.LimitReader(stream, 32<<10))
			_ = stream.Close()
			if err != nil {
				return "", errors.New("log stream failed")
			}
			_, _ = fmt.Fprintf(&out, "[%s/%s]\n%s\n", pod.Name, ct.Name, engine.Redact(string(b), values))
			if out.Len() > 16<<10 {
				return truncateLogTail(out.String()), nil
			}
		}
	}
	return truncateLogTail(out.String()), nil
}

// CompleteLogs accepts at most 16 KiB. Truncate only after known-value
// redaction, so a larger tail cannot turn into a permanently rejected response.
func truncateLogTail(text string) string {
	const limit = 16 << 10
	const marker = "\n[log tail truncated]\n"
	// JSON replaces invalid UTF-8 with U+FFFD; normalize before counting bytes
	// so that decoding the response cannot increase it beyond the API limit.
	text = strings.ToValidUTF8(text, "\uFFFD")
	if len(text) <= limit {
		return text
	}
	end := limit - len(marker)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + marker
}
