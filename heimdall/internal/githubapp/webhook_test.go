package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/heimdall-dev/heimdall/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type captureQueue struct {
	messages []queue.Message
	err      error
}

func (q *captureQueue) Send(_ context.Context, m queue.Message) error {
	q.messages = append(q.messages, m)
	return q.err
}
func signature(secret, body []byte) string {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}
func TestWebhookBoundary(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	body := []byte(`{ "action":"opened", "number":101, "repository":{"id":7}, "installation":{"id":9} }`)
	for _, tc := range []struct {
		name      string
		body      []byte
		signature string
		queueErr  error
		want      int
		count     int
	}{
		{"valid exact bytes", body, signature(secret, body), nil, 202, 1},
		{"modified bytes", append(append([]byte{}, body...), ' '), signature(secret, body), nil, 401, 0},
		{"malformed signature", body, "sha256=nothex", nil, 401, 0},
		{"wrong algorithm", body, "sha1=123", nil, 401, 0},
		{"queue unavailable", body, signature(secret, body), errors.New("offline"), 503, 1},
		{"oversize", []byte(strings.Repeat("x", queue.MaxBodyBytes+1)), "", nil, 413, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &captureQueue{err: tc.queueErr}
			h := &Webhook{Secret: secret, Queue: q}
			r := httptest.NewRequest("POST", "/v1/github/webhook", strings.NewReader(string(tc.body)))
			r.Header.Set("X-Hub-Signature-256", tc.signature)
			r.Header.Set("X-GitHub-Event", "pull_request")
			r.Header.Set("X-GitHub-Delivery", "delivery-101")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || len(q.messages) != tc.count {
				t.Fatalf("status %d/messages %d, want %d/%d", w.Code, len(q.messages), tc.want, tc.count)
			}
			if tc.count > 0 && q.messages[0].Group() != "7#101" {
				t.Fatal("wrong FIFO group")
			}
		})
	}
}
func TestLambdaRawBase64(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	body := []byte("{\n \"action\":\"created\",\"installation\":{\"id\":9},\"repository\":{\"id\":7},\"issue\":{\"number\":102,\"pull_request\":{}},\"comment\":{\"id\":17}\n}")
	q := &captureQueue{}
	h := &Webhook{Secret: secret, Queue: q}
	event := events.APIGatewayV2HTTPRequest{RawPath: "/v1/github/webhook", Body: base64.StdEncoding.EncodeToString(body), IsBase64Encoded: true, Headers: map[string]string{"x-hub-signature-256": signature(secret, body), "x-github-event": "issue_comment", "x-github-delivery": "comment-17"}}
	event.RequestContext.HTTP.Method = "POST"
	response, err := h.HandleLambda(context.Background(), event)
	if err != nil || response.StatusCode != 202 || len(q.messages) != 1 || string(q.messages[0].Payload) != string(body) {
		t.Fatalf("exact base64 body was not preserved: status %d err %v", response.StatusCode, err)
	}
	event.Body = "bad!"
	response, _ = h.HandleLambda(context.Background(), event)
	if response.StatusCode != 400 {
		t.Fatal("malformed base64 accepted")
	}
}

func TestWebhookPreservesTraceAcrossQueueBoundary(t *testing.T) {
	old := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(old)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	traceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := trace.SpanIDFromHex("0123456789abcdef")
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), parent)
	secret := []byte(strings.Repeat("s", 32))
	body := []byte(`{"action":"opened","number":101,"repository":{"id":7},"installation":{"id":9}}`)
	q := &captureQueue{}
	h := &Webhook{Secret: secret, Queue: q}
	r := httptest.NewRequest("POST", "/v1/github/webhook", strings.NewReader(string(body))).WithContext(ctx)
	r.Header.Set("X-Hub-Signature-256", signature(secret, body))
	r.Header.Set("X-GitHub-Event", "pull_request")
	r.Header.Set("X-GitHub-Delivery", "traced-delivery")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 || len(q.messages) != 1 {
		t.Fatal("traced webhook rejected")
	}
	m := q.messages[0]
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	downstream := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": m.TraceParent})
	sc := trace.SpanContextFromContext(downstream)
	if sc.TraceID() != traceID || sc.SpanID() == spanID || !sc.IsRemote() {
		t.Fatalf("trace parent was not linked through webhook span: %s", m.TraceParent)
	}
}
