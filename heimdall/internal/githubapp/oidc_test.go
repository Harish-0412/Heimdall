package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signedToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	part := head + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(part))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return part + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func TestOIDCBoundClaimsAndKeyRotation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kid": "test-key", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	defer server.Close()
	verifier, err := NewOIDCVerifier(OIDCOptions{Audience: "heimdall-test", JWKSURL: server.URL, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"iss": ActionsIssuer, "aud": "heimdall-test", "sub": "repo:team/shop:pull_request", "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "nbf": now.Unix(), "repository_id": "7", "repository": "team/shop", "ref": "refs/pull/101/merge", "sha": strings.Repeat("a", 40), "event_name": "pull_request", "job_workflow_ref": "platform/heimdall/.github/workflows/preview.yml@" + strings.Repeat("c", 40), "job_workflow_sha": strings.Repeat("c", 40), "run_id": "42", "run_attempt": "1"}
	id, err := verifier.Verify(context.Background(), signedToken(t, key, claims))
	if err != nil || id.RepositoryID != 7 || id.PullRequest != 101 {
		t.Fatalf("valid identity rejected: %#v %v", id, err)
	}
	for _, tc := range []struct {
		name, claim string
		value       any
	}{
		{"wrong issuer", "iss", "https://attacker.invalid"}, {"wrong audience", "aud", "another-api"}, {"expired", "exp", now.Add(-time.Second).Unix()}, {"future", "nbf", now.Add(time.Minute).Unix()}, {"excessive lifetime", "exp", now.Add(time.Hour).Unix()}, {"PR target event", "event_name", "pull_request_target"}, {"subject suffix", "sub", "repo:team/shop:pull_request:extra"}, {"branch ref", "ref", "refs/heads/main"}, {"bad SHA", "sha", "latest"}, {"workflow missing", "job_workflow_ref", ""}, {"attempt missing", "run_attempt", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyClaims := map[string]any{}
			for k, v := range claims {
				copyClaims[k] = v
			}
			copyClaims[tc.claim] = tc.value
			if _, err := verifier.Verify(context.Background(), signedToken(t, key, copyClaims)); err == nil {
				t.Fatal("invalid claims accepted")
			}
		})
	}
	token := signedToken(t, key, claims)
	parts := strings.Split(token, ".")
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte("forged"))
	if _, err := verifier.Verify(context.Background(), strings.Join(parts, ".")); err == nil {
		t.Fatal("forged signature accepted")
	}
	if requests != 1 {
		t.Fatalf("JWKS keys not cached, requests %d", requests)
	}
	claims["sub"] = "repo:team/shop:pull_request:job_workflow_ref:" + claims["job_workflow_ref"].(string)
	if _, err := verifier.Verify(context.Background(), signedToken(t, key, claims)); err != nil {
		t.Fatalf("exact workflow-bound custom subject rejected: %v", err)
	}
	claims["sub"] = claims["sub"].(string) + ":extra"
	if _, err := verifier.Verify(context.Background(), signedToken(t, key, claims)); err == nil {
		t.Fatal("extra custom subject claims accepted")
	}
}
