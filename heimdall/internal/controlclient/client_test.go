package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
)

type memorySession struct {
	mu           sync.Mutex
	s            Session
	revision     int
	dropPairOnce bool
}

func (s *memorySession) Load(context.Context) (Session, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s, strconv.Itoa(s.revision), nil
}
func (s *memorySession) Save(_ context.Context, v Session, rev string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rev != strconv.Itoa(s.revision) {
		return errors.New("conflict")
	}
	if s.dropPairOnce && v.Pair.AccessToken != "" {
		s.dropPairOnce = false
		return errors.New("lost persistence response")
	}
	s.s = v
	s.revision++
	return nil
}

func TestCredentialExchangeSurvivesRestartAndLostWrite(t *testing.T) {
	state := &memorySession{s: Session{Enrollment: "bootstrap"}, dropPairOnce: true}
	var nonce string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/register" {
			calls++
			key := r.Header.Get("Idempotency-Key")
			if len(key) != 64 {
				t.Error("nonce notdurable")
			}
			stored, _, _ := state.Load(r.Context())
			if stored.Nonce != key {
				t.Error("nonce was not persisted before exchange")
			}
			if nonce != "" && key != nonce {
				t.Error("lost response must reuse nonce")
			}
			nonce = key
			_ = json.NewEncoder(w).Encode(gen.TokenPair{ClusterID: "cluster", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(10 * time.Minute), RefreshExpiresAt: time.Now().Add(time.Hour)})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Error("wrong bearer")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c, err := New(server.URL, "cluster", "test", state, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("want bounded replay after lost write, got %d exchanges", calls)
	}
	restarted, _ := New(server.URL, "cluster", "test", state, true)
	if err = restarted.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("restart consumed another enrollment")
	}
	s, _, _ := state.Load(context.Background())
	if s.Nonce != "" || s.Enrollment != "" {
		t.Fatal("predecessor and nonce not cleared after durable save")
	}
}

func TestCredentialCannotCrossClusterOrRedirect(t *testing.T) {
	state := &memorySession{s: Session{Pair: gen.TokenPair{ClusterID: "other", AccessToken: "secret", AccessExpiresAt: time.Now().Add(time.Hour)}}}
	c, _ := New("https://example.com", "cluster", "test", state, false)
	if c.Heartbeat(context.Background()) == nil {
		t.Fatal("cross-cluster credential accepted")
	}
	if _, err := New("http://example.com", "cluster", "test", state, false); err == nil {
		t.Fatal("implicit insecure endpoint accepted")
	}
	state.s.Pair.ClusterID = "cluster"
	reached := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(204) }))
	defer dest.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, _ = New(redirect.URL, "cluster", "test", state, true)
	if c.Heartbeat(context.Background()) == nil || reached {
		t.Fatal("bearer redirect followed")
	}
}

func TestConcurrentReplicaRotationUsesOneDurableNonce(t *testing.T) {
	state := &memorySession{s: Session{Enrollment: "bootstrap"}}
	var mu sync.Mutex
	keys := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/register" {
			mu.Lock()
			keys[r.Header.Get("Idempotency-Key")] = true
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(gen.TokenPair{ClusterID: "cluster", AccessToken: "access", RefreshToken: "refresh", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)})
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	a, _ := New(server.URL, "cluster", "test", state, true)
	b, _ := New(server.URL, "cluster", "test", state, true)
	var wg sync.WaitGroup
	for _, c := range []*Client{a, b} {
		wg.Go(func() {
			if err := c.Heartbeat(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(keys) != 1 {
		t.Fatalf("replicas generated %d independent exchange nonces", len(keys))
	}
}
