// Package tracecontext carries only bounded W3C trace identity across durable
// deployment boundaries. Baggage and application data are never persisted.
package tracecontext

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const MaxBytes = 128
const MaxLength = MaxBytes

// Keep persisted values within the same lexical constraint used by the CRD
// and PostgreSQL. The propagator separately enforces version/ID semantics.
var parentPattern = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}(-[0-9a-f]+)*$`)

func Valid(parent string) bool { return parent != "" && Validate(parent) == nil }
func Extract(ctx context.Context, parent string) context.Context {
	updated, err := WithParent(ctx, parent)
	if err != nil {
		return ctx
	}
	return updated
}
func Inject(ctx context.Context, header http.Header) {
	header.Del("traceparent")
	header.Del("tracestate")
	header.Del("baggage")
	if value := Capture(ctx); value != "" {
		header.Set("traceparent", value)
	}
}

// Initialize creates trace identities even when no exporter is installed.
// Exporters and sampling configuration can be attached during P10 rollout.
func Initialize() func(context.Context) error {
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	return provider.Shutdown
}
func Validate(parent string) error {
	if parent == "" {
		return nil
	}
	if len(parent) > MaxBytes {
		return errors.New("trace parent exceeds its limit")
	}
	if !parentPattern.MatchString(parent) {
		return errors.New("invalid W3C trace parent")
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": parent})
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return errors.New("invalid W3C trace parent")
	}
	return nil
}
func Capture(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
func WithParent(ctx context.Context, parent string) (context.Context, error) {
	if err := Validate(parent); err != nil {
		return ctx, err
	}
	if parent == "" {
		return ctx, nil
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": parent}), nil
}
