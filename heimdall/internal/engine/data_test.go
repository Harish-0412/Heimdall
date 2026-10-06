package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/render"
)

func TestImportPolicy(t *testing.T) {
	// SQL programs exercise the gate; no application data is generated or loaded.
	for _, test := range []struct {
		name, sql string
		approved  bool
		want      string
	}{
		{"schema only", "", false, ""},
		{"unapproved", "SELECT current_database();", false, "engine.data_approval"},
		{"approved SQL", "SELECT current_database();", true, ""},
		{"psql reconnect", `\connect postgres`, true, "engine.data_unsafe"},
		{"external connection", "SELECT 'postgres://app@db.production.internal/app';", true, "engine.data_unsafe"},
		{"server escape", "COPY pg_database TO PROGRAM 'cat';", true, "engine.data_unsafe"},
		{"foreign access", "CREATE EXTENSION dblink;", true, "engine.data_unsafe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Spec{Context: render.Context{Seed: []byte(test.sql)}}
			if test.approved {
				digest := sha256.Sum256(s.Context.Seed)
				s.SeedApproval = &SeedApproval{SHA256: hex.EncodeToString(digest[:]), Sanitised: true, ApprovedBy: "test runner", Reason: "SQL gate verification; no imported records"}
			}
			err := validateData(s)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if errorCode(err) != test.want {
				t.Fatalf("got %v, want %s", err, test.want)
			}
		})
	}
}
func TestApprovalBoundToContent(t *testing.T) {
	a := sha256.Sum256([]byte("SELECT current_database();"))
	s := Spec{Context: render.Context{Seed: []byte("SELECT current_user;")}, SeedApproval: &SeedApproval{SHA256: hex.EncodeToString(a[:]), Sanitised: true, ApprovedBy: "test runner", Reason: "SQL gate verification"}}
	if err := validateData(s); errorCode(err) != "engine.data_digest" {
		t.Fatal(err)
	}
}
func TestGeneratedCredentialsAreRedacted(t *testing.T) {
	credentials := render.GenerateCredentials()
	text := "connected with " + credentials.PostgresSuperuser + " postgres://app:" + credentials.PostgresApp + "@postgres/app password=" + credentials.Redis
	got := Redact(text, []string{credentials.PostgresSuperuser, credentials.PostgresApp, credentials.Redis})
	for _, value := range []string{credentials.PostgresSuperuser, credentials.PostgresApp, credentials.Redis} {
		if strings.Contains(got, value) {
			t.Fatal("credential was not redacted")
		}
	}
	// Where it connects is diagnosis, not a secret.
	if !strings.Contains(got, "@postgres/app") {
		t.Errorf("connection context lost: %s", got)
	}
}
