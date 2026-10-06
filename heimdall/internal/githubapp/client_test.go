package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func privateKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
func TestNarrowTokensETagsAndAmbiguousPost(t *testing.T) {
	gets, posts, sleeps := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/9/access_tokens" {
			var input struct {
				Repositories []int64           `json:"repository_ids"`
				Permissions  map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Repositories) != 1 || input.Repositories[0] != 7 {
				t.Error("token repository not narrow")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "test-token", "repositories": []any{map[string]int64{"id": 7}}, "permissions": input.Permissions})
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("installation token not used")
		}
		if r.Method == "GET" {
			gets++
			if gets == 1 {
				w.Header().Set("ETag", "\"first\"")
				_, _ = w.Write([]byte(`{"id":7,"full_name":"team/shop","default_branch":"main"}`))
			} else {
				if r.Header.Get("If-None-Match") != "\"first\"" {
					t.Error("ETag missing")
				}
				w.WriteHeader(304)
			}
			return
		}
		posts++
		w.WriteHeader(500)
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{AppID: 3, PrivateKey: privateKey(t), BaseURL: server.URL, MaxAttempts: 4, Sleep: func(context.Context, time.Duration) error { sleeps++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ref := RepoRef{InstallationID: 9, RepositoryID: 7, FullName: "team/shop"}
	for range 2 {
		r, err := client.Repository(context.Background(), ref)
		if err != nil || r.ID != 7 {
			t.Fatalf("GET/cache failed: %#v %v", r, err)
		}
	}
	if _, err := client.WriteComment(context.Background(), ref, 101, 0, "preview"); err == nil {
		t.Fatal("ambiguous POST unexpectedly passed")
	}
	if posts != 1 || sleeps != 0 {
		t.Fatalf("ambiguous create was retried: posts %d sleeps %d", posts, sleeps)
	}
}
func TestConfigRefAndPermission(t *testing.T) {
	if !CanMaintain("write") || !CanMaintain("maintain") || !CanMaintain("admin") || CanMaintain("read") || CanMaintain("triage") {
		t.Fatal("permission gate is incorrect")
	}
	client, err := NewClient(ClientOptions{AppID: 1, PrivateKey: privateKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Config(context.Background(), RepoRef{InstallationID: 1, RepositoryID: 7, FullName: "team/shop"}, "main"); err == nil {
		t.Fatal("mutable config ref accepted")
	}
	if ValidSHA(strings.Repeat("A", 40)) || !ValidSHA(strings.Repeat("a", 40)) {
		t.Fatal("SHA syntax gate incorrect")
	}
}

func TestVerifyBuildBindsCurrentPRRunAndReusableWorkflow(t *testing.T) {
	key := privateKey(t)
	head := strings.Repeat("a", 40)
	merge := strings.Repeat("b", 40)
	workflowSHA := strings.Repeat("c", 40)
	workflowRef := "platform/heimdall/.github/workflows/preview.yml@" + workflowSHA
	for _, name := range []string{"valid", "old head", "fork", "closed", "workflow ref", "workflow SHA", "run attempt", "run SHA", "run repository", "run PR link"} {
		t.Run(name, func(t *testing.T) {
			prHead, state, headRepo := head, "open", int64(7)
			runAttempt, runRepo, runPR, runSHA := int64(1), int64(7), int64(101), merge
			identity := ActionsIdentity{RepositoryID: 7, Repository: "team/shop", PullRequest: 101, SHA: merge, WorkflowRef: workflowRef, WorkflowSHA: workflowSHA, RunID: "42", RunAttempt: 1}
			switch name {
			case "old head":
				prHead = strings.Repeat("f", 40)
			case "fork":
				headRepo = 99
			case "closed":
				state = "closed"
			case "workflow ref":
				identity.WorkflowRef = "unreviewed/workflow@main"
			case "workflow SHA":
				identity.WorkflowSHA = strings.Repeat("f", 40)
			case "run attempt":
				runAttempt = 2
			case "run SHA":
				runSHA = head
			case "run repository":
				runRepo = 99
			case "run PR link":
				runPR = 102
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/access_tokens") {
					var input map[string]any
					_ = json.NewDecoder(r.Body).Decode(&input)
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "token", "permissions": input["permissions"], "repositories": []any{map[string]int64{"id": 7}}})
					return
				}
				if r.URL.Path == "/repos/team/shop/pulls/101" {
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 101, "state": state, "head": map[string]any{"sha": prHead, "repo": map[string]int64{"id": headRepo}}, "base": map[string]any{"repo": map[string]int64{"id": 7}}})
					return
				}
				if r.URL.Path == "/repos/team/shop/actions/runs/42" {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "run_attempt": runAttempt, "head_sha": runSHA, "event": "pull_request", "repository": map[string]int64{"id": runRepo}, "head_repository": map[string]int64{"id": headRepo}, "pull_requests": []any{map[string]any{"number": runPR, "head": map[string]any{"sha": head, "repo": map[string]int64{"id": 7}}}}})
					return
				}
				t.Errorf("unexpected endpoint %s", r.URL.Path)
				w.WriteHeader(404)
			}))
			defer server.Close()
			c, err := NewClient(ClientOptions{AppID: 3, PrivateKey: key, BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.VerifyBuild(context.Background(), RepoRef{InstallationID: 9, RepositoryID: 7, FullName: "team/shop"}, identity, head, workflowRef, workflowSHA)
			if (err == nil) != (name == "valid") {
				t.Fatalf("case %s result %v", name, err)
			}
		})
	}
}
