package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/heimdall-dev/heimdall/internal/render"
)

// This conservative SQL gate is defence in depth, not a sanitisation service.
// Human approval establishes provenance; the database role and network policy
// enforce isolation. Reject psql commands, external endpoints and server escape
// mechanisms even in approved imports.
var unsafeSQL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://|\b(host|hostaddr|service)\s*=|\bdblink\b|\bforeign\s+(server|data|table)\b|\bcopy\b|\bcreate\s+extension\b|\bprogram\b|\b(prod|production)[._-])`)

func validateData(s Spec) error {
	if len(s.Context.Seed) == 0 {
		return nil
	}
	a := s.SeedApproval
	if a == nil || !a.Sanitised || strings.TrimSpace(a.ApprovedBy) == "" || strings.TrimSpace(a.Reason) == "" {
		return failure("engine.data_approval", "seed data requires operator approval of sanitised data; synthetic data is prohibited", nil)
	}
	sum := sha256.Sum256(s.Context.Seed)
	if a.SHA256 != hex.EncodeToString(sum[:]) {
		return failure("engine.data_digest", "approved digest does not match the imported data", nil)
	}
	if len(s.Context.Seed) > render.MaxSeedBytes || !utf8.Valid(s.Context.Seed) || strings.ContainsAny(string(s.Context.Seed), "\\\x00") || unsafeSQL.Match(s.Context.Seed) {
		return failure("engine.data_unsafe", "seed contains unsupported SQL, external connection information or psql commands", nil)
	}
	return nil
}
