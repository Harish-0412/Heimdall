//go:build integration

package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store/storetest"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func redisFixture(t *testing.T) *redis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "public.ecr.aws/docker/library/redis:8.2.2-alpine", ExposedPorts: []string{"6379/tcp"}, Cmd: []string{"redis-server", "--save", "", "--appendonly", "no"}, WaitingFor: wait.ForListeningPort("6379/tcp")}, Started: true})
	if err != nil {
		t.Fatalf("required real volatile Redis: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Terminate(ctx); err != nil {
			t.Errorf("terminate Redis: %v", err)
		}
	})
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = client.Close() })
	if err = client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRedisLogBrokerAtomicLimitsAndTTL(t *testing.T) {
	client := redisFixture(t)
	broker := NewRedisLogBroker(client)
	checkBrokerConcurrency(t, broker)
	ctx := context.Background()
	r := brokerRecord("short-lived", 100*time.Millisecond)
	r.Request.EnvironmentID = "other-env"
	if err := broker.Create(ctx, r, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := broker.Get(ctx, r.Request.Id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired tail remains readable: %v", err)
	}
	appendOnly, err := client.ConfigGet(ctx, "appendonly").Result()
	if err != nil || appendOnly["appendonly"] != "no" {
		t.Fatalf("log Redis append-only persistence is enabled: %v", err)
	}
	save, err := client.ConfigGet(ctx, "save").Result()
	if err != nil || save["save"] != "" {
		t.Fatalf("log Redis snapshot persistence is enabled: %v", err)
	}
	if err := broker.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfigSet(ctx, "save", "60 1").Err(); err != nil {
		t.Fatal(err)
	}
	if broker.Ready(ctx) == nil {
		t.Fatal("broker readiness accepted enabled snapshot persistence")
	}
	w := apiRequest(t, New(apiFixture(), Options{LogBroker: broker}), "GET", "/readyz", "", "", nil)
	if w.Code != 503 {
		t.Fatal("API readiness accepted a persistent log broker")
	}
	if err := client.ConfigSet(ctx, "save", "").Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisLogsAcrossTwoAPIReplicas(t *testing.T) {
	client := redisFixture(t)
	f := apiFixture()
	first := New(f, Options{LogBroker: NewRedisLogBroker(client)})
	second := New(f, Options{LogBroker: NewRedisLogBroker(client)})
	w := apiRequest(t, first, "POST", "/v1/environments/env-7/logs", "member", `{"workload":"app","tail":10}`, nil)
	var request gen.LogRequest
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &request) != nil {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, second, "GET", "/v1/agent/log-requests", "agent", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), request.Id) {
		t.Fatal("agent cannot poll request from another API replica: " + w.Body.String())
	}
	w = apiRequest(t, second, "POST", "/v1/agent/log-requests/"+request.Id, "agent", `{"generation":1,"redacted":true,"text":"known values already redacted; token=must-redact-again"}`, nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, first, "GET", "/v1/log-requests/"+request.Id, "member", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "must-redact-again") || !strings.Contains(w.Body.String(), "completed") {
		t.Fatal("user cannot read response from another API replica: " + w.Body.String())
	}
}

func TestHTTPWithRealPostgresLifecycleFencingAndRevocation(t *testing.T) {
	f := storetest.New(t)
	ctx := context.Background()
	issue := func(p domain.Principal) domain.IssuedCredential {
		c, err := f.Store.IssueCredential(ctx, p, domain.CredentialInput{ActorID: p.ActorID, Role: "admin", Kind: "user", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	user := issue(f.Principal)
	other := issue(f.Other)
	server := New(f.Store, Options{PollInterval: 10 * time.Millisecond, StreamDuration: time.Second})
	spec := apiSpec()
	spec.Tenant = f.Principal.TenantSlug
	spec.Repository = f.Repository.FullName
	raw, _ := json.Marshal(spec)
	body, _ := json.Marshal(gen.CreateEnvironment{ClusterID: f.Cluster.ID, RepositoryID: f.Repository.ID, Name: spec.EnvironmentID, Spec: raw})
	w := apiRequest(t, server, "POST", "/v1/environments", user.Token, string(body), map[string]string{"Idempotency-Key": "http-create-42"})
	var environment gen.Environment
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &environment) != nil {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = apiRequest(t, server, "GET", "/v1/environments/"+environment.Id, other.Token, "", nil)
	if w.Code != 404 {
		t.Fatal("cross-tenant resource exposure: " + w.Body.String())
	}
	w = apiRequest(t, server, "POST", "/v1/environments", user.Token, string(body), map[string]string{"Idempotency-Key": "http-create-42"})
	if w.Code != 201 {
		t.Fatal("lost-response create replay failed: " + w.Body.String())
	}
	enrollment, err := f.Store.IssueCredential(ctx, f.Principal, domain.CredentialInput{ActorID: "cluster:" + f.Cluster.ID, ClusterID: f.Cluster.ID, Role: "agent", Kind: "enrollment", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	w = apiRequest(t, server, "POST", "/v1/agent/register", enrollment.Token, `{"clusterID":"`+f.Cluster.ID+`","version":"test"}`, map[string]string{"Idempotency-Key": "persisted-enrollment"})
	var pair gen.TokenPair
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &pair) != nil {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, server, "POST", "/v1/agent/register", enrollment.Token, `{"clusterID":"`+f.Cluster.ID+`","version":"test"}`, map[string]string{"Idempotency-Key": "persisted-enrollment"})
	var replay gen.TokenPair
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.AccessToken != pair.AccessToken || replay.RefreshToken != pair.RefreshToken {
		t.Fatal("registration lost-response retry failed")
	}
	push := func(phase string, generation int64) {
		t.Helper()
		payload, _ := json.Marshal(gen.StatusUpdate{EventID: "event-" + phase + "-" + time.Now().Format("150405.000000"), Generation: generation, Version: environment.Version, Phase: gen.StatusUpdatePhase(phase), Status: json.RawMessage(`{"phase":"` + phase + `"}`)})
		w = apiRequest(t, server, "POST", "/v1/agent/environments/"+environment.Id+"/status", pair.AccessToken, string(payload), nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &environment) != nil {
			t.Fatalf("%s: %d %s", phase, w.Code, w.Body.String())
		}
	}
	push("Provisioning", 1)
	push("Ready", 1)
	for _, action := range []string{"reset", "retry", "delete"} {
		payload, _ := json.Marshal(gen.Action{Action: gen.ActionAction(action), Version: environment.Version})
		w = apiRequest(t, server, "POST", "/v1/environments/"+environment.Id+"/actions", user.Token, string(payload), map[string]string{"Idempotency-Key": "http-action-" + action})
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &environment) != nil {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
		}
		stale, _ := json.Marshal(gen.StatusUpdate{EventID: "stale-" + action, Generation: 1, Version: 1, Phase: gen.StatusUpdatePhase("Ready"), Status: json.RawMessage(`{"phase":"Ready"}`)})
		w = apiRequest(t, server, "POST", "/v1/agent/environments/"+environment.Id+"/status", pair.AccessToken, string(stale), nil)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "state.stale_generation") {
			t.Fatalf("stale report accepted: %d %s", w.Code, w.Body.String())
		}
		if action == "retry" {
			push("Provisioning", environment.Generation)
		}
		if action == "delete" {
			push("Destroyed", environment.Generation)
		} else {
			push("Ready", environment.Generation)
		}
	}
	w = apiRequest(t, server, "GET", "/v1/environments/"+environment.Id+"/events?limit=100", user.Token, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"stale"`) || !strings.Contains(w.Body.String(), "environment.reset") {
		t.Fatalf("persistent timeline incomplete: %s", w.Body.String())
	}
	streamServer := httptest.NewServer(server.Handler())
	defer streamServer.Close()
	r, _ := http.NewRequest("GET", streamServer.URL+"/v1/environments/"+environment.Id+"/timeline", nil)
	r.Header.Set("Authorization", "Bearer "+user.Token)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	start := time.Now()
	if err = f.Store.RevokeCredential(ctx, f.Principal, user.ID); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("revoked SSE session remained open")
	}
	w = apiRequest(t, server, "POST", "/v1/clusters/"+f.Cluster.ID+"/revoke", other.Token, "", nil)
	if w.Code != 404 {
		t.Fatal(w.Body.String())
	}
	if err = f.Store.RevokeClusterCredentials(ctx, f.Principal, f.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	w = apiRequest(t, server, "GET", "/v1/agent/desired", pair.AccessToken, "", nil)
	if w.Code != 401 {
		t.Fatal("cluster revocation left agent access alive")
	}
}
