package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/controlclient"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestStatusNotificationLinksDeploymentTrace(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) }()
	parent := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("traceparent")
		if r.Header.Get("baggage") != "" || r.Header.Get("tracestate") != "" {
			t.Error("private metadata propagated")
		}
		_ = json.NewEncoder(w).Encode(gen.Environment{Id: "preview", Version: 2})
	}))
	defer server.Close()
	session := &reporterSession{session: controlclient.Session{Pair: gen.TokenPair{ClusterID: "cluster", AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}}}
	control, err := controlclient.New(server.URL, "cluster", "test", session, true)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(v1.PreviewEnvironmentSpec{TraceParent: parent})
	r := &Reporter{Control: control, last: map[string]string{}, versions: map[string]int64{}}
	if err = r.reportStatus(context.Background(), gen.Environment{Id: "preview", Generation: 1, Version: 1, Spec: spec}, v1.PreviewEnvironmentStatus{Phase: v1.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	original := trace.SpanContextFromContext(tracecontext.Extract(context.Background(), parent))
	actual := trace.SpanContextFromContext(tracecontext.Extract(context.Background(), received))
	if !actual.IsValid() || actual.TraceID() != original.TraceID() || actual.SpanID() == original.SpanID() {
		t.Fatal("status HTTP callback did not preserve a deployment child trace")
	}
}
