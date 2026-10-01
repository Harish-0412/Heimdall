package render

import (
	"fmt"
	"strings"
)

// Error codes are a public contract, like config diagnostic codes: tooling and
// the diagnostics engine match on them, so they are never reused or renamed.
const (
	CodeConfigNotLoaded    = "render.config.not_loaded"
	CodeContextInvalid     = "render.context.invalid"
	CodeCredentialsInvalid = "render.credentials.invalid"
	CodeImageMissing       = "render.image.missing"
	CodeImageUnpinned      = "render.image.unpinned"
	CodeImageLatest        = "render.image.latest"
	CodeImageMismatch      = "render.image.mismatch"
	CodeImageUnknown       = "render.image.unknown"
	CodeSeedMissing        = "render.seed.missing"
	CodeSeedUnexpected     = "render.seed.unexpected"
	CodeSeedTooLarge       = "render.seed.too_large"
	CodeEnvConflict        = "render.env.conflict"
	CodeResourcesInvalid   = "render.resources.invalid"
)

// Error is one problem with Render's input.
type Error struct {
	Code string
	// Field names the offending input, for example "Context.Repo" or
	// "Context.Images.api". Empty when the problem is not field-specific.
	Field   string
	Message string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s [%s]", e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s [%s]", e.Field, e.Message, e.Code)
}

// Errors collects every problem found in one pass, so a caller fixing its
// input sees all of them at once. Render returns it as its error.
type Errors []*Error

func (es Errors) Error() string {
	msgs := make([]string, len(es))
	for i, e := range es {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}

// Codes returns the error codes in order. Handy in tests.
func (es Errors) Codes() []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Code
	}
	return out
}

func (es *Errors) add(code, field, format string, args ...any) {
	*es = append(*es, &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)})
}

// err returns es as an error, or nil when it is empty (avoiding the classic
// non-nil interface holding a nil slice).
func (es Errors) err() error {
	if len(es) == 0 {
		return nil
	}
	return es
}
