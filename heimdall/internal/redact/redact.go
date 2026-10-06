// Package redact removes secrets from text that leaves a preview: logs shown
// to users, diagnostics evidence, PR comments.
//
// The primary control is exact: the agent knows every value it injected
// (generated credentials, the tenant's secrets), and those values are
// replaced wherever they appear, including in their common encodings.
// Pattern scanning is a safety net for values Heimdall did not inject; it is
// deliberately conservative and keeps what diagnosis needs (hosts, ports,
// SQL states, table names). Previews carry no powerful cloud credentials by
// default (P7), so the net is a second line, not the first.
package redact

import (
	"cmp"
	"encoding/base64"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Marker replaces a redacted value.
const Marker = "[REDACTED]"

// Redactor redacts a fixed set of known values plus patterns. The zero value
// redacts patterns only. It is safe for concurrent use.
type Redactor struct {
	known *strings.Replacer
}

// New returns a Redactor for the given known secret values. Multiline values
// (PEM keys) are also redacted line by line, and every value also in its
// base64 and URL-encoded forms.
func New(values ...string) *Redactor {
	seen := map[string]bool{}
	var known []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			known = append(known, v)
		}
	}
	for _, v := range values {
		add(v)
		for _, line := range strings.Split(v, "\n") {
			add(strings.TrimSpace(line))
		}
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(base64.RawStdEncoding.EncodeToString([]byte(v)))
		add(base64.URLEncoding.EncodeToString([]byte(v)))
		add(base64.RawURLEncoding.EncodeToString([]byte(v)))
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
	}
	// Preserve existing markers across repeated sanitization, even when a
	// short secret is a substring of the marker itself.
	add(Marker)
	slices.SortFunc(known, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	pairs := make([]string, 0, 2*len(known))
	for _, v := range known {
		pairs = append(pairs, v, Marker)
	}
	return &Redactor{known: strings.NewReplacer(pairs...)}
}

// String redacts known values, then patterns.
func (r *Redactor) String(text string) string {
	if r != nil && r.known != nil {
		text = r.known.Replace(text)
	}
	return Patterns(text)
}

// Lines redacts each line and caps the result at maxLines lines (the last
// ones) and maxBytes bytes, marking what was cut.
func (r *Redactor) Lines(text string, maxLines, maxBytes int) string {
	if text == "" {
		return ""
	}
	// Redact before truncation and splitting: otherwise a partial exact value
	// or the body of a multiline private key could survive into the tail.
	lines := strings.Split(strings.TrimRight(r.String(text), "\n"), "\n")
	cut := 0
	if maxLines > 0 && len(lines) > maxLines {
		cut = len(lines) - maxLines
		lines = lines[cut:]
	}
	out := strings.Join(lines, "\n")
	if maxBytes > 0 && len(out) > maxBytes {
		// Keep the end: the failure is usually last.
		out = out[len(out)-maxBytes:]
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
		cut = -1
	}
	switch {
	case cut > 0:
		out = "[... " + strconv.Itoa(cut) + " earlier lines omitted]\n" + out
	case cut < 0:
		out = "[... earlier output omitted]\n" + out
	}
	return out
}

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys, whole blocks or a lone header line.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), Marker},
	// user:password@ in URLs: keep scheme, user and host for diagnosis.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@"'<>]*):[^\s@/"'<>]+@`), "${1}:" + Marker + "@"},
	// Authorization headers and bearer tokens.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Marker},
	// key=value and "key": "value" for secret-looking keys.
	{regexp.MustCompile(`(?i)\b((?:[a-z0-9_.-]*[_.-])?(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|authorization|auth)["']?\s*[:=]\s*["']?)[^\s,;"'&}]+`), "${1}" + Marker},
	// Row values PostgreSQL prints in error details: data, not diagnosis.
	{regexp.MustCompile(`(Failing row contains )\(.*\)`), "${1}(" + Marker + ")"},
	{regexp.MustCompile(`(Key \([^)]*\)=)\([^)]*\)`), "${1}(" + Marker + ")"},
	// Well-known token formats.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Marker},    // JWT
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Marker},                                   // AWS access key id
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})\b`), Marker}, // GitHub
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), Marker},                                // Slack
	{regexp.MustCompile(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}\b`), Marker},                        // Stripe
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Marker},                                       // Google API key
	{regexp.MustCompile(`\b(?:sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,})\b`), Marker},                    // LLM API keys
}

// Patterns applies only the pattern safety net.
func Patterns(text string) string {
	for _, p := range patterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	return text
}
