package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

type diagnoserFunc func(context.Context, engine.Spec, diagnose.Failure) (*diagnose.Report, error)

func (f diagnoserFunc) Diagnose(ctx context.Context, spec engine.Spec, failure diagnose.Failure) (*diagnose.Report, error) {
	return f(ctx, spec, failure)
}

func TestDiagnosisSurvivesCollectionFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *diagnose.Report
		err    error
	}{
		{"API unavailable", nil, errors.New("API unavailable")},
		{"no report", nil, nil},
		{"empty report", &diagnose.Report{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{Diagnoser: diagnoserFunc(func(_ context.Context, _ engine.Spec, f diagnose.Failure) (*diagnose.Report, error) {
				if f.Step != "baseline-db/migrate" {
					t.Errorf("lost failed step: %+v", f)
				}
				return tc.report, tc.err
			})}
			spec := engine.Spec{Context: render.Context{Repo: "acme/demo", PR: 7, URLSuffix: "abcd", Generation: 3}}
			result := &engine.Result{Events: []engine.Event{{Generation: 3, Stage: "baseline-db/migrate", State: "failed", Code: "engine.timeout"}}}
			failure := &engine.Error{Code: "engine.timeout", Message: "step exceeded deadline"}
			err := r.explain(context.Background(), spec, result, failure)
			if !errors.Is(err, failure) {
				t.Fatalf("lost original failure: %v", err)
			}
			report := reportOf(err)
			if report == nil || report.RootCause() == nil || report.RootCause().Code != diagnose.Unclassified || report.RootCause().Stage != "baseline-db" {
				t.Fatalf("fallback report: %+v", report)
			}
		})
	}
}

func TestStatusDiagnosesRespectCRDLimits(t *testing.T) {
	report := &diagnose.Report{}
	for range 7 {
		d := diagnose.Diagnosis{Code: diagnose.Unclassified, Summary: strings.Repeat("界", 1200), Subject: strings.Repeat("界", 300),
			Stage: strings.Repeat("x", 100), Suggestion: strings.Repeat("界", 1200)}
		for range 15 {
			d.Evidence = append(d.Evidence, strings.Repeat("界", 300))
		}
		report.Diagnoses = append(report.Diagnoses, d)
	}
	status := statusDiagnoses(report)
	if len(status) != 5 {
		t.Fatalf("kept %d diagnoses", len(status))
	}
	for i, d := range status {
		if len([]rune(d.Summary)) > 1024 || len([]rune(d.Subject)) > 253 || len([]rune(d.Stage)) > 64 || len([]rune(d.Suggestion)) > 1024 {
			t.Errorf("diagnosis %d exceeds schema limits", i)
		}
		if i == 0 && len(d.Evidence) != 10 || i > 0 && len(d.Evidence) != 0 {
			t.Errorf("evidence count at %d: %d", i, len(d.Evidence))
		}
		for _, e := range d.Evidence {
			if len([]rune(e)) > 256 {
				t.Errorf("evidence exceeds schema limit: %d", len([]rune(e)))
			}
		}
	}
}
