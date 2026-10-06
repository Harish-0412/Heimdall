package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/heimdall-dev/heimdall/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
)

type Sender interface {
	Send(context.Context, queue.Message) error
}
type Webhook struct {
	Secret []byte
	Queue  Sender
}

// VerifySignature verifies the exact request bytes before JSON parsing.
func VerifySignature(secret, body []byte, signature string) bool {
	if len(secret) < 32 || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write(body)
	return hmac.Equal(h.Sum(nil), want)
}
func DecodeWebhook(event, delivery string, body []byte) (*queue.Message, error) {
	var p struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
		Number int64  `json:"number"`
		Action string `json:"action"`
		Issue  struct {
			Number      int64           `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errors.New("invalid webhook JSON")
	}
	switch event {
	case "pull_request":
		if p.Action != "opened" && p.Action != "synchronize" && p.Action != "reopened" && p.Action != "closed" {
			return nil, nil
		}
	case "issue_comment":
		if p.Action != "created" || len(p.Issue.PullRequest) == 0 || string(p.Issue.PullRequest) == "null" {
			return nil, nil
		}
		p.Number = p.Issue.Number
	default:
		return nil, nil
	}
	m := &queue.Message{DeliveryID: delivery, Event: event, InstallationID: p.Installation.ID, RepositoryID: p.Repository.ID, PullRequest: p.Number, Payload: json.RawMessage(body)}
	return m, m.Validate()
}
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("heimdall.github").Start(r.Context(), "github.webhook")
	defer span.End()
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if len(h.Secret) < 32 || h.Queue == nil {
		http.Error(w, "webhook unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, queue.MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "invalid request body", 400)
		return
	}
	if len(body) > queue.MaxBodyBytes {
		http.Error(w, "webhook too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !VerifySignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	m, err := DecodeWebhook(r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery"), body)
	if err != nil {
		http.Error(w, "invalid webhook identity", 400)
		return
	}
	if m == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	span.SetAttributes(attribute.Int64("github.repository.id", m.RepositoryID), attribute.Int64("github.pull_request", m.PullRequest), attribute.String("github.delivery_id", m.DeliveryID))
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	m.TraceParent = carrier.Get("traceparent")
	if err := h.Queue.Send(ctx, *m); err != nil {
		http.Error(w, "queue unavailable; retry delivery", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
