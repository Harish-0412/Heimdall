package redact

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// Planted secrets: every one must disappear, in every form an app might
// print it, while the context diagnosis needs stays.
func TestKnownValuesInEveryForm(t *testing.T) {
	password := "Zq8v2Lk9Wm4Xp7Rt" // as render.GenerateCredentials makes them
	special := "p@ss/w:rd+&=more"  // URL-significant characters
	pem := "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBmYp1wH3fR8\nAoGCCqGSM49AwEHoUQDQgAE\n-----END EC PRIVATE KEY-----"
	r := New(password, special, pem)
	text := strings.Join([]string{
		"connecting with " + password,
		"DATABASE_URL=postgres://app:" + url.QueryEscape(special) + "@postgres:5432/app",
		"b64 " + base64.StdEncoding.EncodeToString([]byte(password)),
		"path " + url.PathEscape(special),
		"key line AoGCCqGSM49AwEHoUQDQgAE in a stack trace",
	}, "\n")
	got := r.String(text)
	for _, secret := range []string{password, special, url.QueryEscape(special), url.PathEscape(special),
		base64.StdEncoding.EncodeToString([]byte(password)), "AoGCCqGSM49AwEHoUQDQgAE"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived:\n%s", secret, got)
		}
	}
	for _, keep := range []string{"connecting with", "@postgres:5432/app", "postgres://app:", "in a stack trace"} {
		if !strings.Contains(got, keep) {
			t.Errorf("context %q lost:\n%s", keep, got)
		}
	}
}

func TestShortKnownSecretsAreRedacted(t *testing.T) {
	if got := New("abc", "42", "").String("secret abc and 42"); got != "secret [REDACTED] and [REDACTED]" {
		t.Errorf("short secrets leaked: %q", got)
	}
}

func TestReplacementDoesNotRedactItsOwnMarker(t *testing.T) {
	r := New("REDACTED", "secret-REDACTED-value")
	if got := r.String("secret-REDACTED-value REDACTED"); got != Marker+" "+Marker {
		t.Errorf("recursive replacement corrupted output: %q", got)
	}
	if got := r.String(Marker); got != Marker {
		t.Errorf("repeated sanitization corrupted marker: %q", got)
	}
}

func TestURLSafeBase64KnownValues(t *testing.T) {
	secret := "secret\xff\xfe\xfb"
	r := New(secret)
	for _, encoding := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding} {
		encoded := encoding.EncodeToString([]byte(secret))
		if got := r.String("value " + encoded); got != "value "+Marker {
			t.Errorf("encoded secret leaked: %q", got)
		}
	}
}

func TestLinesRedactsMultilineBeforeTruncation(t *testing.T) {
	text := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow-private-body\nmore-private-body\n-----END RSA PRIVATE KEY-----\nafter\n"
	if got := New().Lines(text, 2, 100); got != Marker+"\nafter" {
		t.Errorf("multiline key tail leaked: %q", got)
	}
	if got := New("alpha\nbeta").Lines("before\nalpha\nbeta\nafter", 2, 100); strings.Contains(got, "beta") {
		t.Errorf("truncated known value leaked: %q", got)
	}
}

// The safety net: values Heimdall did not inject.
func TestPatterns(t *testing.T) {
	stripeFixture := "sk_" + "live_" + strings.Repeat("fixture", 5)
	for _, tc := range []struct{ in, secret, keep string }{
		{"redis://default:hunter2hunter2@redis:6379/0", "hunter2hunter2", "@redis:6379/0"},
		{"amqp://guest:s3cr3t-value@rabbitmq:5672", "s3cr3t-value", "@rabbitmq:5672"},
		{"Authorization: Bearer abcdefghijklmnop.qrstu", "abcdefghijklmnop", "Authorization"},
		{`{"password":"opensesame","user":"app"}`, "opensesame", `"user":"app"`},
		{"DB_PASSWORD=opensesame exit", "opensesame", "exit"},

		{"stripe_api_key: " + stripeFixture, stripeFixture, "stripe_api_key"},
		{"aws AKIAIOSFODNN7EXAMPLE used", "AKIAIOSFODNN7EXAMPLE", "used"},
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123456789AB", "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB", "token"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "eyJhbGciOiJIUzI1NiJ9", "jwt"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\nafter", "MIIEow", "after"},
		{"x-api-key=abcdef123456 next", "abcdef123456", "next"},
		{"DETAIL:  Failing row contains (7, alice@example.com, null).", "alice@example.com", "Failing row contains"},
		{"DETAIL:  Key (email)=(bob@example.com) already exists.", "bob@example.com", "Key (email)="},
	} {
		got := Patterns(tc.in)
		if strings.Contains(got, tc.secret) || !strings.Contains(got, tc.keep) {
			t.Errorf("Patterns(%q) = %q", tc.in, got)
		}
	}
}

// Diagnosis evidence must survive the net untouched.
func TestPatternsKeepDiagnosticContext(t *testing.T) {
	for _, s := range []string{
		`error: column "owner_id" of relation "catalog" contains null values`,
		"code: '23502',",
		"connect ECONNREFUSED 10.96.12.4:5432",
		"Back-off pulling image \"localhost:5003/shopflow-api@sha256:0123\"",
		"Liveness probe failed: HTTP probe failed with statuscode: 500",
		"exceeded quota: heimdall-quota, requested: limits.memory=512Mi, used: limits.memory=1Gi, limited: limits.memory=1Gi",
		"author=octocat tokens: 5",
	} {
		if got := Patterns(s); got != s {
			t.Errorf("changed diagnostic text:\n  in:  %s\n  out: %s", s, got)
		}
	}
}

func TestLinesCapsAndMarksTheCut(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	got := New().Lines(b.String(), 10, 0)
	if !strings.HasPrefix(got, "[... 90 earlier lines omitted]\n") || strings.Count(got, "\n") != 10 {
		t.Errorf("line cap:\n%s", got)
	}
	got = New().Lines(strings.Repeat("abcdefghij\n", 50), 0, 64)
	if !strings.HasPrefix(got, "[... earlier output omitted]\n") || len(got) > 64+len("[... earlier output omitted]\n") {
		t.Errorf("byte cap (%d bytes):\n%s", len(got), got)
	}
	if New().Lines("", 10, 10) != "" {
		t.Error("empty input")
	}
}
