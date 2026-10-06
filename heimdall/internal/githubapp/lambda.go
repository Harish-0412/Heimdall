package githubapp

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/heimdall-dev/heimdall/internal/queue"
)

type lambdaResponse struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *lambdaResponse) Header() http.Header { return w.header }
func (w *lambdaResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *lambdaResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}

// HandleLambda preserves the exact bytes API Gateway received, including a
// binary/base64 body. Never re-marshal JSON before verifying GitHub's HMAC.
func (h *Webhook) HandleLambda(ctx context.Context, event events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	if len(event.Body) > base64.StdEncoding.EncodedLen(queue.MaxBodyBytes)+4 {
		return events.APIGatewayV2HTTPResponse{StatusCode: 413, Body: "webhook too large"}, nil
	}
	body := []byte(event.Body)
	if event.IsBase64Encoded {
		var err error
		body, err = base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 400, Body: "invalid request body"}, nil
		}
	}
	r, err := http.NewRequestWithContext(ctx, event.RequestContext.HTTP.Method, "https://webhook.invalid"+event.RawPath, strings.NewReader(string(body)))
	if err != nil {
		return events.APIGatewayV2HTTPResponse{StatusCode: 400, Body: "invalid request"}, nil
	}
	for key, value := range event.Headers {
		r.Header.Set(key, value)
	}
	w := &lambdaResponse{header: make(http.Header)}
	h.ServeHTTP(w, r)
	if w.status == 0 {
		w.status = 200
	}
	headers := map[string]string{}
	for key := range w.header {
		headers[key] = w.header.Get(key)
	}
	return events.APIGatewayV2HTTPResponse{StatusCode: w.status, Headers: headers, Body: w.body.String()}, nil
}
