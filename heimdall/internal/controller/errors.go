// Package controller reconciles PreviewEnvironment objects with the engine
// (P2). Engine operations take minutes, so Reconcile never waits for them:
// a Runner executes them asynchronously, and their progress and results come
// back as reconcile requests (ADR 0010).
package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/heimdall-dev/heimdall/internal/engine"
)

// Failure is a classified error with a stable public code (cross-cutting
// rule: typed errors, no stack traces or secrets in user-facing text).
type Failure struct {
	Code      string
	Message   string
	Step      string
	Retryable bool
	cause     error
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }
func (f *Failure) Unwrap() error { return f.cause }

// Codes the agent adds to the engine's.
const (
	CodeConfigDigest    = "agent.config_digest"
	CodeConfigInvalid   = "agent.config_invalid"
	CodeDataMissing     = "agent.data_missing"
	CodeDataUnexpected  = "agent.data_unexpected"
	CodeDataUnavailable = "agent.data_unavailable"
	CodeAccess          = "agent.access"
	CodeInterrupted     = "agent.interrupted"
	// CodeBusy is the engine's: another holder has the journal lease.
	CodeBusy    = "engine.busy"
	CodeCluster = "engine.cluster"
)

// terminal engine codes need new input (a higher generation, a new reset
// nonce, or an operator) rather than a retry: repeating the same operation
// would fail the same way, or could destroy evidence.
var terminal = map[string]bool{
	"engine.spec_invalid":        true,
	"engine.job_failed":          true,
	"engine.image":               true,
	"engine.data_approval":       true,
	"engine.data_digest":         true,
	"engine.data_unsafe":         true,
	"engine.credentials":         true,
	"engine.credentials_missing": true,
	"engine.credentials_invalid": true,
	"engine.ownership":           true,
	"engine.namespace":           true,
	"engine.generation_conflict": true,
	"engine.stale_generation":    true,
	"engine.stale_reset":         true,
	"engine.reset_nonce":         true,
	"engine.not_ready":           true,
	"engine.storage":             true,
	"engine.journal_invalid":     true,
	"engine.workload_failed":     true,
	"engine.delete_fenced":       true,
	CodeConfigDigest:             true,
	CodeConfigInvalid:            true,
	CodeDataMissing:              true,
	CodeDataUnexpected:           true,
}

// classify turns any error from an operation into a Failure. Unknown errors
// are retryable with a generic message: the raw error (which may quote API
// responses) goes to logs, not to the object's status.
func classify(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	var e *engine.Error
	if errors.As(err, &e) && e.Code == "engine.lock_lost" {
		return &Failure{Code: e.Code, Message: e.Message, Retryable: true, cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Failure{Code: CodeInterrupted, Message: "operation was interrupted before it finished", Retryable: true, cause: err}
	}
	if errors.As(err, &e) {
		return &Failure{Code: e.Code, Message: e.Message, Retryable: !terminal[e.Code], cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Code: "engine.timeout", Message: "a step exceeded its deadline", Retryable: true, cause: err}
	}
	return &Failure{Code: CodeCluster, Message: "a Kubernetes API operation failed; see the agent logs", Retryable: true, cause: err}
}

func failf(code string, retryable bool, format string, args ...any) *Failure {
	return &Failure{Code: code, Message: fmt.Sprintf(format, args...), Retryable: retryable}
}
