package controller

import (
	"context"
	"errors"
	"time"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// Diagnoser explains a failed operation (internal/diagnose): it captures a
// snapshot of the preview and ranks the likely causes.
type Diagnoser interface {
	Diagnose(ctx context.Context, spec engine.Spec, failure diagnose.Failure) (*diagnose.Report, error)
}

// diagnoseTimeout bounds a diagnosis; the operation's concurrency slot is
// held meanwhile.
const diagnoseTimeout = 30 * time.Second

// maxDiagnoses kept in status (the CRD's limit).
const maxDiagnoses = 5

// diagnosed carries a failed operation's diagnosis to consume. It unwraps to
// the failure itself, so classification is unaffected.
type diagnosed struct {
	err    error
	report *diagnose.Report
}

func (d *diagnosed) Error() string { return d.err.Error() }
func (d *diagnosed) Unwrap() error { return d.err }

// explain diagnoses err when it is a real failure (not cancellation or a
// held lock) and the reconciler has a Diagnoser. It returns err, wrapped
// with the report when there is one.
func (r *Reconciler) explain(ctx context.Context, spec engine.Spec, result *engine.Result, err error) error {
	if err == nil || ctx.Err() != nil || r.Diagnoser == nil {
		return err
	}
	f := classify(err)
	if f.Code == CodeBusy || f.Code == CodeInterrupted {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
	defer cancel()
	failure := diagnose.Failure{Code: f.Code, Message: f.Message, Step: f.Step}
	if failure.Step == "" && result != nil {
		for i := len(result.Events) - 1; i >= 0; i-- {
			ev := result.Events[i]
			if ev.State == "failed" && ev.Code == failure.Code && ev.Generation == spec.Context.Generation {
				failure.Step = ev.Stage
				break
			}
		}
	}
	report, derr := r.Diagnoser.Diagnose(dctx, spec, failure)
	if derr != nil || report == nil || report.RootCause() == nil {
		// Collection is best effort, but an operation failure must still
		// have a stable diagnosis even when the cluster cannot be inspected.
		ns := render.NamespaceFor(spec.Context.Repo, spec.Context.PR, spec.Context.URLSuffix)
		report = diagnose.Diagnose(diagnose.ForFailure(ns, spec.Context.Generation, &failure, derr))
	}
	return &diagnosed{err: err, report: report}
}

// reportOf returns the diagnosis attached to err, if any.
func reportOf(err error) *diagnose.Report {
	var d *diagnosed
	if errors.As(err, &d) {
		return d.report
	}
	return nil
}

// configReport diagnoses a spec rejected before any operation started: the
// configuration's own diagnostics are the evidence; nothing is read from the
// cluster.
func configReport(err error, diags config.Diagnostics) *diagnose.Report {
	f := classify(err)
	snap := &diagnose.Snapshot{Version: diagnose.SnapshotVersion, Failure: &diagnose.Failure{Code: f.Code, Message: f.Message}}
	for _, d := range diags {
		if d.Severity == config.SeverityError {
			snap.ConfigProblems = append(snap.ConfigProblems, diagnose.ConfigProblem{Code: d.Code, Path: d.Path, Line: d.Line, Message: d.Message})
		}
	}
	snap.Sanitize(redact.New())
	return diagnose.Diagnose(snap)
}

// statusDiagnoses converts a report for status: the top few, with evidence
// only for the root cause.
func statusDiagnoses(r *diagnose.Report) []v1alpha1.DiagnosisStatus {
	if r == nil {
		return nil
	}
	var out []v1alpha1.DiagnosisStatus
	for i, d := range r.Diagnoses {
		if i == maxDiagnoses {
			break
		}
		s := v1alpha1.DiagnosisStatus{Code: string(d.Code), Summary: clip(d.Summary, 1024), Subject: clip(d.Subject, 253),
			Stage: clip(d.Stage, 64), Suggestion: clip(d.Suggestion, 1024)}
		if i == 0 {
			for _, e := range d.Evidence[:min(len(d.Evidence), 10)] {
				s.Evidence = append(s.Evidence, clip(e, 256))
			}
		}
		out = append(out, s)
	}
	return out
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
