package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/controlclient"
	"github.com/heimdall-dev/heimdall/internal/source"
)

type reporterControl struct{ snapshot gen.DesiredSnapshot }

func (c *reporterControl) Desired(context.Context) (gen.DesiredSnapshot, error) {
	return c.snapshot, nil
}

type reporterSession struct{ session controlclient.Session }

func (s *reporterSession) Load(context.Context) (controlclient.Session, string, error) {
	return s.session, "1", nil
}
func (s *reporterSession) Save(context.Context, controlclient.Session, string) error {
	return errors.New("test credentials must not rotate")
}

func TestReporterPublishesSafePreparationFailureAndRecovers(t *testing.T) {
	ctx := context.Background()
	var reports []gen.StatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/log-requests":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/agent/environments/preview/status":
			var update gen.StatusUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
			}
			reports = append(reports, update)
			_ = json.NewEncoder(w).Encode(gen.Environment{Id: "preview", Version: update.Version + 1})
		default:
			t.Errorf("unexpected outbound route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	session := &reporterSession{session: controlclient.Session{Pair: gen.TokenPair{ClusterID: "cluster", AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}}}
	control, err := controlclient.New(server.URL, "cluster", "test", session, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultPolicy()
	policy.AllowedSecrets, policy.AllowedRegistries = []string{}, []string{"registry.test/previews/"}
	spec := v1.PreviewEnvironmentSpec{EnvironmentID: "preview", Tenant: "acme", Generation: 1, Config: v1.ConfigSource{Inline: "config", SHA256: bundle.ConfigDigest([]byte("config")), Bundle: "registry.test/previews/bundle@sha256:" + strings.Repeat("a", 64)}}
	b, _ := json.Marshal(spec)
	snapshotControl := &reporterControl{snapshot: gen.DesiredSnapshot{TenantID: "tenant", TenantSlug: "acme", Revision: "1", Policy: policy, Environments: []gen.Environment{{Id: "preview", Name: "preview", ClusterID: "cluster", TenantID: "tenant", Generation: 1, Version: 1, DesiredState: gen.EnvironmentDesiredStateRunning, Spec: b}}}}
	api := &source.API{Control: snapshotControl, ClusterID: "cluster", Interval: time.Second, LoadBundle: func(context.Context, string, string) (*bundle.Contents, error) {
		return nil, errors.New("registry password super-secret fixture SQL detail")
	}}
	snapshot, err := api.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Prepare(ctx, &snapshot.Environments[0]); err == nil {
		t.Fatal("test did not produce a preparation failure")
	}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	reporter := &Reporter{Control: control, Source: api, Reader: k, Namespace: "agent"}
	if err := reporter.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Generation != 1 || reports[0].Phase != gen.StatusUpdatePhaseFailed || strings.Contains(string(reports[0].Status), "super-secret") || strings.Contains(string(reports[0].Status), "SQL detail") {
		t.Fatalf("unsafe or missing first-build failure: %+v", reports)
	}
	var failure v1.PreviewEnvironmentStatus
	if err := json.Unmarshal(reports[0].Status, &failure); err != nil || failure.LastError == nil || !failure.LastError.Retryable || len(failure.Diagnoses) != 1 {
		t.Fatalf("preparation diagnosis is incomplete: %v %+v", err, failure)
	}
	if err := reporter.Report(ctx); err != nil || len(reports) != 1 {
		t.Fatalf("unchanged preparation failure was not deduplicated: %v", err)
	}
	api.LoadBundle = func(context.Context, string, string) (*bundle.Contents, error) {
		return &bundle.Contents{Config: []byte("config")}, nil
	}
	if err := api.Prepare(ctx, &snapshot.Environments[0]); err != nil {
		t.Fatal(err)
	}
	pe := &v1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: "preview", Generation: 1, Labels: map[string]string{source.LabelSource: "api"}}, Spec: spec, Status: v1.PreviewEnvironmentStatus{Phase: v1.PhaseProvisioning, ObservedGeneration: 1}}
	if err := k.Create(ctx, pe); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(ctx); err != nil || len(reports) != 2 || reports[1].Phase != gen.StatusUpdatePhaseProvisioning || reports[1].Version != 2 {
		t.Fatalf("recovery did not resume runtime reporting with CAS version: %v %+v", err, reports)
	}
	// The artifact was already validated and projected. A later registry outage
	// cannot replace this generation's actual cluster status with a source error.
	api.LoadBundle = func(context.Context, string, string) (*bundle.Contents, error) {
		return nil, errors.New("registry offline")
	}
	_ = api.Prepare(ctx, &snapshot.Environments[0])
	if err := reporter.Report(ctx); err != nil || len(reports) != 2 {
		t.Fatalf("registry outage overwrote an existing projection: %v %+v", err, reports)
	}
	// A late Ready result from this CR cannot report success for a new generation.
	snapshotControl.snapshot.Revision = "2"
	snapshotControl.snapshot.Environments[0].Generation = 2
	if _, err := api.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(ctx); err != nil || len(reports) != 2 {
		t.Fatalf("old projection crossed a generation fence: %v %+v", err, reports)
	}
}

func TestReporterLogRequestsRejectConflictingClusterProjection(t *testing.T) {
	for _, test := range []struct {
		name, environmentID, source string
	}{{"wrong environment", "another", "api"}, {"unmanaged object", "preview", "manual"}} {
		t.Run(test.name, func(t *testing.T) {
			var result *gen.LogResult
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/agent/log-requests":
					_ = json.NewEncoder(w).Encode([]gen.LogRequest{{Id: "request", EnvironmentID: "preview", Generation: 1, Workload: "web", Tail: 10, ExpiresAt: time.Now().Add(time.Minute)}})
				case "/v1/agent/log-requests/request":
					result = new(gen.LogResult)
					if err := json.NewDecoder(r.Body).Decode(result); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			session := &reporterSession{session: controlclient.Session{Pair: gen.TokenPair{ClusterID: "cluster", AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}}}
			control, err := controlclient.New(server.URL, "cluster", "test", session, true)
			if err != nil {
				t.Fatal(err)
			}
			scheme := runtime.NewScheme()
			_ = v1.AddToScheme(scheme)
			pe := &v1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: "preview", Labels: map[string]string{source.LabelSource: test.source}}, Spec: v1.PreviewEnvironmentSpec{EnvironmentID: test.environmentID, Generation: 1}}
			k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pe).Build()
			reporter := &Reporter{Control: control, Reader: k, Namespace: "agent"}
			if err := reporter.logs(context.Background(), map[string]gen.Environment{"preview": {Id: "preview", Name: "preview", Generation: 1}}); err != nil {
				t.Fatal(err)
			}
			if result == nil || result.Error == nil || result.Text != nil || !result.Redacted {
				t.Fatalf("conflicting projection was allowed to disclose logs: %+v", result)
			}
		})
	}
}

func TestRequestedLogTailFitsControlAPIBudgetWithoutSplittingUTF8(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", 16<<10), strings.Repeat("x", 32<<10), strings.Repeat("界", 16<<10), strings.Repeat("x\xff", 8<<10)} {
		out := truncateLogTail(text)
		if len(out) > 16<<10 || !utf8.ValidString(out) {
			t.Fatalf("log tail exceeded its protocol bound or broke UTF-8: %d", len(out))
		}
		if len(strings.ToValidUTF8(text, "\uFFFD")) > 16<<10 && !strings.HasSuffix(out, "\n[log tail truncated]\n") {
			t.Fatal("truncated response did not explain its limit")
		}
		if utf8.ValidString(text) && len(text) <= 16<<10 && text != out {
			t.Fatal("a tail within the bound changed")
		}
	}
}
