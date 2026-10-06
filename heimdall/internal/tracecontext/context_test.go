package tracecontext

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestBoundedTraceIdentityRoundTrip(t *testing.T) {
	value := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	ctx := Extract(context.Background(), value)
	if !trace.SpanContextFromContext(ctx).IsRemote() {
		t.Fatal("remote parent was lost")
	}
	headers := http.Header{"Baggage": {"private=value"}, "Tracestate": {"private=value"}, "Traceparent": {"stale"}}
	Inject(ctx, headers)
	if headers.Get("traceparent") != value || headers.Get("baggage") != "" || headers.Get("tracestate") != "" {
		t.Fatal("trace identity changed or extra metadata propagated")
	}
	for _, bad := range []string{"", "invalid", strings.Repeat("a", 129), "00-00000000000000000000000000000000-0123456789abcdef-01", "01-0123456789abcdef0123456789abcdef-0123456789abcdef-01-unstructured"} {
		if Valid(bad) || trace.SpanContextFromContext(Extract(context.Background(), bad)).IsValid() {
			t.Fatal("invalid trace parent accepted")
		}
	}
}
