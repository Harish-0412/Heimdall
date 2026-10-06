package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTraceMetadataDoesNotChangeEngineIdentity(t *testing.T) {
	doc := "version: 1\nservices:\n  app:\n    image: ghcr.io/acme/app@sha256:" + strings.Repeat("a", 64) + "\n    port: 8080\n"
	pe := &v1.PreviewEnvironment{Spec: v1.PreviewEnvironmentSpec{Tenant: "acme", Repository: "acme/demo", PullRequest: 7, Commit: strings.Repeat("a", 40), Generation: 1, EnvironmentID: "preview", Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)), Config: v1.ConfigSource{Inline: doc, SHA256: bundle.ConfigDigest([]byte(doc))}}}
	b := SpecBuilder{Policy: config.DefaultPolicy(), Platform: render.Platform{BaseDomain: "preview.example.com"}}
	first, _, err := b.Build(context.Background(), pe)
	if err != nil {
		t.Fatal(err)
	}
	pe.Spec.TraceParent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	second, _, err := b.Build(context.Background(), pe)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("trace metadata changed deployment identity: %v", err)
	}
	pe.Spec.TraceParent = strings.Repeat("a", 129)
	if _, err := b.Validate(context.Background(), pe); err == nil {
		t.Fatal("admission accepted oversized trace metadata")
	}
	pe.Spec.DesiredState = v1.DesiredDestroyed
	if _, _, err := b.Build(context.Background(), pe); err != nil {
		t.Fatal("bad telemetry prevented safe cleanup", err)
	}
}

func TestRunnerCarriesTraceButRetainsLeadershipCancellation(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) }()
	parent := tracecontext.Extract(context.Background(), "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	parent, cancelParent := context.WithCancel(parent)
	cancelParent()
	manager, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()
	runner := NewRunner(func(engine.Observer) Operator { return nil }, 1, func(types.NamespacedName) {})
	runner.base = manager
	started := make(chan trace.SpanContext, 1)
	op := runner.LaunchContext(parent, types.NamespacedName{Name: "preview"}, opKey{Type: v1.OperationApply, Generation: 1}, time.Now(), func(ctx context.Context, _ Operator) (*engine.Result, error) {
		started <- trace.SpanContextFromContext(ctx)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	select {
	case sc := <-started:
		original := trace.SpanContextFromContext(parent)
		if sc.TraceID() != original.TraceID() || sc.SpanID() == original.SpanID() {
			t.Fatal("asynchronous engine operation lost its child trace")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile cancellation incorrectly cancelled the independent operation")
	}
	cancelManager()
	select {
	case <-op.done:
	case <-time.After(3 * time.Second):
		t.Fatal("operation outlived manager leadership")
	}
}
