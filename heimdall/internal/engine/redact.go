package engine

import "github.com/heimdall-dev/heimdall/internal/redact"

// Redact removes the exact injected values first, in their common encodings;
// patterns only cover values Heimdall did not inject (internal/redact).
func Redact(text string, values []string) string {
	return redact.New(values...).String(text)
}
