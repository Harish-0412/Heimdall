package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
)

func TestLogsAreScopedBoundedRedactedAndTransient(t *testing.T) {
	f := apiFixture()
	s := New(f, Options{LogTTL: 100 * time.Millisecond})
	w := apiRequest(t, s, "POST", "/v1/environments/env-7/logs", "member", `{"workload":"app","tail":20}`, nil)
	var request gen.LogRequest
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &request) != nil {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	w = apiRequest(t, s, "GET", "/v1/log-requests/"+request.Id, "other-tenant", "", nil)
	if w.Code != 404 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "GET", "/v1/agent/log-requests", "other-cluster", "", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "POST", "/v1/agent/log-requests/"+request.Id, "agent", `{"generation":1,"redacted":false,"text":"secret"}`, nil)
	if w.Code != 400 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "POST", "/v1/agent/log-requests/"+request.Id, "other-cluster", `{"generation":1,"redacted":true,"text":"ok"}`, nil)
	if w.Code != 404 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "POST", "/v1/agent/log-requests/"+request.Id, "agent", `{"generation":2,"redacted":true,"text":"ok"}`, nil)
	if w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "POST", "/v1/agent/log-requests/"+request.Id, "agent", `{"generation":1,"redacted":true,"text":"password=never-store-this-tail"}`, nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, s, "GET", "/v1/log-requests/"+request.Id, "member", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-store-this-tail") || !strings.Contains(w.Body.String(), "REDACTED") {
		t.Fatal(w.Body.String())
	}
	for _, audit := range f.audits {
		if strings.Contains(string(audit), "password") || strings.Contains(string(audit), "REDACTED") || strings.Contains(string(audit), "tail") {
			t.Fatal("log bytes entered audit metadata")
		}
	}
	time.Sleep(120 * time.Millisecond)
	w = apiRequest(t, s, "GET", "/v1/log-requests/"+request.Id, "member", "", nil)
	if w.Code != 404 {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{`{"workload":"postgres"}`, `{"workload":"app","tail":201}`, `{"workload":"app","tail":0}`} {
		w = apiRequest(t, s, "POST", "/v1/environments/env-7/logs", "member", body, nil)
		if w.Code != 400 {
			t.Fatal(w.Body.String())
		}
	}
}

func brokerRecord(id string, ttl time.Duration) LogRecord {
	return LogRecord{TenantID: tenantA, ClusterID: clusterA, ActorID: "octocat", Request: gen.LogRequest{Id: id, EnvironmentID: "env-7", Generation: 1, Workload: "app", Tail: 20, State: gen.LogRequestStatePending, ExpiresAt: time.Now().Add(ttl)}}
}

func checkBrokerConcurrency(t *testing.T, b LogBroker) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := b.Create(ctx, brokerRecord(fmt.Sprintf("request-%02d", i), time.Minute), time.Minute)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrLogLimit) {
				t.Errorf("create: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 4 {
		t.Fatalf("atomic environment limit accepted %d, want 4", accepted.Load())
	}
	rows, err := b.Pending(ctx, tenantA, clusterA)
	if err != nil || len(rows) != 4 {
		t.Fatalf("pending: %d %v", len(rows), err)
	}
	wrong := rows[0]
	wrong.ClusterID = clusterB
	wrong.Request.State = gen.LogRequestStateCompleted
	if !errors.Is(b.Complete(ctx, wrong), domain.ErrStaleGeneration) {
		t.Fatal("cross-cluster completion accepted")
	}
	complete := rows[0]
	complete.Request.State = gen.LogRequestStateCompleted
	value := "redacted tail"
	complete.Request.Text = &value
	if err = b.Complete(ctx, complete); err != nil {
		t.Fatal(err)
	}
	row, err := b.Get(ctx, complete.Request.Id)
	if err != nil || row.Request.Text == nil || *row.Request.Text != value {
		t.Fatalf("completion: %+v %v", row, err)
	}
	complete.Request.Text = nil
	if err = b.Complete(ctx, complete); err != nil {
		t.Fatal(err)
	}
	row, err = b.Get(ctx, complete.Request.Id)
	if err != nil || row.Request.Text == nil || *row.Request.Text != value {
		t.Fatal("completion replay overwrote first response")
	}
}

func TestMemoryLogBrokerAtomicBoundsAndCompletion(t *testing.T) {
	checkBrokerConcurrency(t, NewMemoryLogBroker())
}
