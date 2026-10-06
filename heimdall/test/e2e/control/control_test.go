//go:build integration

package controle2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/controlapi"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/controlclient"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store/storetest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type live struct {
	t         *testing.T
	f         *storetest.Fixture
	k         client.Client
	context   string
	user      string
	apiURL    string
	images    map[string]string
	doc       string
	seedSHA   string
	bundleRef string
	offline   atomic.Bool
	handler   atomic.Value
}

func TestControlPlaneExitCriteria(t *testing.T) {
	kubeContext := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if kubeContext == "" {
		t.Skip("run test/e2e/control/run.sh with Docker and kind")
	}
	if !strings.HasPrefix(kubeContext, "kind-") {
		t.Fatal("dedicated kind context required")
	}
	e := &live{t: t, context: kubeContext, f: storetest.New(t)}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	e.must(err)
	scheme := runtime.NewScheme()
	e.must(clientgoscheme.AddToScheme(scheme))
	e.must(v1alpha1.AddToScheme(scheme))
	e.k, err = client.New(rc, client.Options{Scheme: scheme})
	e.must(err)
	doc, err := os.ReadFile("heimdall.yaml")
	e.must(err)
	e.doc = string(doc)
	seed, err := os.ReadFile("seed.sql")
	e.must(err)
	e.seedSHA = bundle.ConfigDigest(seed)
	imageData, err := os.ReadFile(os.Getenv("HEIMDALL_E2E_IMAGES"))
	e.must(err)
	e.must(json.Unmarshal(imageData, &e.images))
	ctx := context.Background()
	policy, err := e.f.Store.GetPolicy(ctx, e.f.Principal)
	e.must(err)
	policy.AllowedRegistries = []string{os.Getenv("HEIMDALL_E2E_REGISTRY") + "/", os.Getenv("HEIMDALL_E2E_BUNDLE_REGISTRY") + "/"}
	e.must(e.f.Store.SetPolicy(ctx, e.f.Principal, policy))
	credential, err := e.f.Store.IssueCredential(ctx, e.f.Principal, domain.CredentialInput{ActorID: e.f.Principal.ActorID, Role: "admin", Kind: "user", TTL: time.Hour})
	e.must(err)
	e.user = credential.Token
	e.handler.Store(controlapi.New(e.f.Store, controlapi.Options{PollInterval: 200 * time.Millisecond}).Handler())
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.offline.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"code":"api.unavailable","message":"temporary test outage"}`)
			return
		}
		e.handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	_ = server.Listener.Close()
	server.Listener, err = net.Listen("tcp4", "0.0.0.0:0")
	e.must(err)
	server.Start()
	t.Cleanup(server.Close)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	e.apiURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	agentURL := "http://" + os.Getenv("HEIMDALL_E2E_API_HOST") + fmt.Sprintf(":%d", port)
	// A real OCI config artifact lives solely in the customer's registry.
	work := t.TempDir()
	e.must(os.WriteFile(filepath.Join(work, "heimdall.yaml"), doc, 0600))
	e.must(os.WriteFile(filepath.Join(work, "seed.sql"), seed, 0600))
	layout := filepath.Join(work, "oci")
	_, err = bundle.Pack(ctx, work, "heimdall.yaml", layout)
	e.must(err)
	ref, err := bundle.Push(ctx, layout, os.Getenv("HEIMDALL_E2E_REGISTRY")+"/control-bundle:e2e", bundle.RegistryOptions{PlainHTTP: true})
	e.must(err)
	e.bundleRef = strings.Replace(ref, os.Getenv("HEIMDALL_E2E_REGISTRY"), os.Getenv("HEIMDALL_E2E_BUNDLE_REGISTRY"), 1)
	fetched, err := bundle.Fetch(ctx, ref, bundle.ConfigDigest(doc), bundle.RegistryOptions{PlainHTTP: true})
	e.must(err)
	if !bytes.Equal(fetched.Config, doc) || !bytes.Equal(fetched.Seed, seed) {
		t.Fatal("customer OCI bundle did not preserve reviewed config and fixture bytes")
	}
	t.Logf("customer OCI bundle %s matches config SHA %s and synthetic fixture SHA %s", e.bundleRef, bundle.ConfigDigest(doc), e.seedSHA)
	e.install(agentURL)
	t.Cleanup(func() {
		if t.Failed() {
			e.diagnostics()
		}
	})
	one := e.create("env-501", 501)
	one = e.ready(one.Id, one.Generation, 10*time.Minute)
	e.fixtureOwned(one)
	t.Log("API create reached Ready through outbound agent with digest-pinned OCI config")
	one = e.action(one, "retry")
	one = e.ready(one.Id, one.Generation, 10*time.Minute)
	var actual v1alpha1.PreviewEnvironment
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: one.Name}, &actual))
	namespace := actual.Status.Namespace
	one = e.action(one, "reset")
	one = e.ready(one.Id, one.Generation, 10*time.Minute)
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: one.Name}, &actual))
	if actual.Status.CompletedResetNonce != 1 {
		t.Fatalf("real reset did not complete: %+v", actual.Status.Operation)
	}
	t.Log("API retry and reset converged; live database reset nonce acknowledged")
	e.timeline(one.Id)
	// A second environment shares infrastructure while retaining its own intent.
	two := e.create("env-502", 502)
	two = e.ready(two.Id, two.Generation, 10*time.Minute)
	e.fixtureOwned(two)
	var second v1alpha1.PreviewEnvironment
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: two.Name}, &second))
	if second.Status.Namespace == namespace {
		t.Fatal("two pull requests share a preview namespace")
	}
	e.tail(two.Id)
	e.extendWithoutReset(two)
	// A first-build registry failure is visible through the API even before a
	// PreviewEnvironment exists, and can still be deleted without its bundle.
	missing := e.createWithBundle("env-503", 503, e.bundleRef[:strings.LastIndex(e.bundleRef, "@")+1]+"sha256:"+strings.Repeat("0", 64))
	missing = e.wait(missing.Id, "Failed", missing.Generation, time.Minute)
	if !strings.Contains(string(missing.Status), "source.bundle_unavailable") {
		t.Fatalf("missing bundle has no stable API diagnosis: %s", missing.Status)
	}
	missing = e.action(missing, "delete")
	e.wait(missing.Id, "Destroyed", missing.Generation, time.Minute)
	t.Log("unavailable first-build OCI bundle produced an API diagnosis and cleanup remained possible")
	// Restart both agent replicas: the consumed enrollment is gone, so only
	// the durable cluster session can restore communication.
	e.command("kubectl", "--context", e.context, "-n", "heimdall-system", "rollout", "restart", "deployment/heimdall-agent")
	e.command("kubectl", "--context", e.context, "-n", "heimdall-system", "rollout", "status", "deployment/heimdall-agent", "--timeout=5m")
	var secret corev1.Secret
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: "heimdall-agent-auth"}, &secret))
	var session controlclient.Session
	e.must(json.Unmarshal(secret.Data["session.json"], &session))
	if len(secret.Data["enrollment"]) != 0 || session.Pair.AccessToken == "" || session.Nonce != "" {
		t.Fatal("enrollment was not replaced by a durable completed session")
	}
	e.handler.Store(controlapi.New(e.f.Store, controlapi.Options{}).Handler())
	e.timeline(one.Id) // History survives replacement of the API process.
	// Authoritative API outage exceeds several sweep passes and the configured
	// grace period. Unknown desired state must never become an empty set.
	e.offline.Store(true)
	time.Sleep(35 * time.Second)
	var ns corev1.Namespace
	e.must(e.k.Get(ctx, types.NamespacedName{Name: namespace}, &ns))
	e.must(e.k.Get(ctx, types.NamespacedName{Name: second.Status.Namespace}, &ns))
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: one.Name}, &actual))
	if actual.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatal("source outage changed desired state")
	}
	e.offline.Store(false)
	two = e.ready(two.Id, two.Generation, 2*time.Minute)
	t.Log("agent and API restart recovered; API outage preserved both previews")
	one = e.read(one.Id)
	one = e.action(one, "delete")
	e.wait(one.Id, "Destroyed", one.Generation, 5*time.Minute)
	e.eventually("deleted preview namespace fully removed", 2*time.Minute, func() bool { return apierrors.IsNotFound(e.k.Get(ctx, types.NamespacedName{Name: namespace}, &ns)) })
	var fixture corev1.ConfigMap
	e.eventually("deleted preview fixture removed", time.Minute, func() bool {
		return apierrors.IsNotFound(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: e.fixtureName(one.Name)}, &fixture))
	})
	two = e.ready(two.Id, two.Generation, time.Minute)
	e.fixtureOwned(two)
	// Cleanup must remain possible after an administrator tightens policy so
	// the already-deployed config would no longer be admitted for new work.
	originalMaxServices := policy.MaxServices
	policy.MaxServices = 1 // ShopFlow declares two services.
	e.request("PUT", "/v1/policy", policy, "", 200, nil)
	time.Sleep(3 * time.Second) // Both webhook replicas pull the new policy.
	two = e.action(two, "delete")
	e.wait(two.Id, "Destroyed", two.Generation, 5*time.Minute)
	policy.MaxServices = originalMaxServices
	e.request("PUT", "/v1/policy", policy, "", 200, nil)
	t.Log("API delete removed first preview while the second stayed Ready; cleanup also succeeded after policy tightened")
}

func (e *live) install(agentURL string) {
	var enrollment gen.Enrollment
	e.request("POST", "/v1/clusters/"+e.f.Cluster.ID+"/enrollment", nil, "", 201, &enrollment)
	ctx := context.Background()
	e.must(e.k.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-system"}}))
	e.must(e.k.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-agent-auth", Namespace: "heimdall-system"}, Data: map[string][]byte{"enrollment": []byte(enrollment.Token)}}))
	image := strings.Split(os.Getenv("HEIMDALL_E2E_AGENT_IMAGE"), "@")
	if len(image) != 2 {
		e.t.Fatal("pinned agent image required")
	}
	e.command("helm", "upgrade", "--install", "heimdall-agent", os.Getenv("HEIMDALL_E2E_CHART"), "--kube-context", e.context, "--namespace", "heimdall-system", "--set", "replicaCount=2", "--set", "platform.baseDomain=preview.test", "--set", "platform.urlScheme=http", "--set", "operations.stepTimeout=6m", "--set", "operations.resyncInterval=20s", "--set", "operations.maxBackoff=20s", "--set", "source.type=api", "--set", "source.syncInterval=2s", "--set-string", "source.controlPlane.url="+agentURL, "--set-string", "source.controlPlane.clusterID="+e.f.Cluster.ID, "--set", "source.controlPlane.allowLocalHTTP=true", "--set", "source.controlPlane.registryPlainHTTP=true", "--set", "sweeper.interval=5s", "--set", "sweeper.gracePeriod=20s", "--set-string", "image.repository="+image[0], "--set-string", "image.digest="+image[1], "--wait", "--timeout", "5m")
	var deployment appsv1.Deployment
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: "heimdall-agent"}, &deployment))
	if deployment.Status.ReadyReplicas != 2 {
		e.t.Fatalf("expected two ready outbound agent replicas, got %d", deployment.Status.ReadyReplicas)
	}
	e.t.Logf("two outbound agent replicas are Ready with image %s", os.Getenv("HEIMDALL_E2E_AGENT_IMAGE"))
}

func (e *live) create(name string, pr int64) gen.Environment {
	return e.createWithBundle(name, pr, e.bundleRef)
}

func (e *live) createWithBundle(name string, pr int64, bundleRef string) gen.Environment {
	spec := v1alpha1.PreviewEnvironmentSpec{Tenant: e.f.Principal.TenantSlug, Repository: e.f.Repository.FullName, PullRequest: pr, Commit: strings.Repeat("a", 40), Generation: 1, EnvironmentID: name, Owner: "octocat", URLSuffix: fmt.Sprintf("x%d", pr), ExpiresAt: metav1.NewTime(time.Now().Add(48 * time.Hour)), DesiredState: v1alpha1.DesiredRunning, Images: e.images, Config: v1alpha1.ConfigSource{Inline: e.doc, SHA256: bundle.ConfigDigest([]byte(e.doc)), Bundle: bundleRef}}
	spec.Data = &v1alpha1.DataSource{ConfigMap: e.fixtureName(name), Key: "seed.sql", Approval: v1alpha1.DataApproval{SHA256: e.seedSHA, ApprovedBy: e.f.Principal.ActorID, Reason: "Reviewed synthetic public acceptance fixture; no customer or production data", Sanitised: true}}
	raw, err := json.Marshal(spec)
	e.must(err)
	var env gen.Environment
	e.request("POST", "/v1/environments", gen.CreateEnvironment{ClusterID: e.f.Cluster.ID, RepositoryID: e.f.Repository.ID, Name: name, Spec: raw}, "create-"+name, 201, &env)
	if strings.Contains(string(env.Spec), "INSERT INTO products") {
		e.t.Fatal("fixture SQL escaped the customer registry into central API metadata")
	}
	return env
}

func (e *live) fixtureName(name string) string {
	return "heimdall-data-" + name + "-" + e.seedSHA[:12]
}

func (e *live) fixtureOwned(env gen.Environment) {
	ctx := context.Background()
	e.eventually("immutable customer fixture bound to exact preview owner", 30*time.Second, func() bool {
		var pe v1alpha1.PreviewEnvironment
		var fixture corev1.ConfigMap
		if e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: env.Name}, &pe) != nil || e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: e.fixtureName(env.Name)}, &fixture) != nil {
			return false
		}
		return fixture.Immutable != nil && *fixture.Immutable && fixture.Labels["heimdall.dev/env"] == env.Id && fixture.Labels["heimdall.dev/source"] == "api" && bundle.ConfigDigest([]byte(fixture.Data["seed.sql"])) == e.seedSHA && len(fixture.OwnerReferences) == 1 && fixture.OwnerReferences[0].UID == pe.UID && fixture.OwnerReferences[0].Kind == "PreviewEnvironment"
	})
}

func (e *live) read(id string) gen.Environment {
	var out gen.Environment
	e.request("GET", "/v1/environments/"+id, nil, "", 200, &out)
	return out
}
func (e *live) action(env gen.Environment, kind string) gen.Environment {
	env = e.read(env.Id)
	var out gen.Environment
	action := gen.Action{Action: gen.ActionAction(kind), Version: env.Version}
	if kind == "extend" {
		expiry := env.ExpiresAt.Add(time.Hour)
		action.ExpiresAt = &expiry
	}
	e.request("POST", "/v1/environments/"+env.Id+"/actions", action, fmt.Sprintf("%s-%s-%d", kind, env.Id, env.Generation), 200, &out)
	return out
}
func (e *live) ready(id string, generation int64, within time.Duration) gen.Environment {
	return e.wait(id, "Ready", generation, within)
}
func (e *live) wait(id, phase string, generation int64, within time.Duration) gen.Environment {
	var out gen.Environment
	var last string
	e.eventually(id+" "+phase, within, func() bool {
		out = e.read(id)
		var st v1alpha1.PreviewEnvironmentStatus
		_ = json.Unmarshal(out.Status, &st)
		if out.Phase != last {
			e.t.Logf("%s generation %d: %s", id, out.Generation, out.Phase)
			last = out.Phase
		}
		if out.Phase == "Failed" {
			e.t.Logf("runtime failure: %s", out.Status)
		}
		return out.Phase == phase && out.Generation == generation && (phase != "Ready" || st.DeployedGeneration == generation || st.CompletedResetNonce == out.ResetNonce && out.ResetNonce > 0)
	})
	return out
}

func (e *live) timeline(id string) {
	var page gen.EventPage
	e.request("GET", "/v1/environments/"+id+"/events?limit=100", nil, "", 200, &page)
	data, _ := json.Marshal(page.Items)
	for _, stage := range []string{"guardrails/", "dependencies/", "baseline-db/", "application/", "smoke/"} {
		if !strings.Contains(string(data), stage) {
			e.t.Fatalf("timeline missing %s: %s", stage, data)
		}
	}
	if page.Next < 1 {
		e.t.Fatal("timeline has no durable cursor")
	}
	var resumed gen.EventPage
	e.request("GET", fmt.Sprintf("/v1/environments/%s/events?after=%d", id, page.Next), nil, "", 200, &resumed)
	for _, event := range resumed.Items {
		if event.Id <= page.Next {
			e.t.Fatal("resumed timeline repeated acknowledged event")
		}
	}
}

func (e *live) tail(id string) {
	var request gen.LogRequest
	e.request("POST", "/v1/environments/"+id+"/logs", gen.LogInput{Workload: "api"}, "", 202, &request)
	e.eventually("on-demand agent log tail", 30*time.Second, func() bool {
		var out gen.LogRequest
		e.request("GET", "/v1/log-requests/"+request.Id, nil, "", 200, &out)
		if out.State == gen.LogRequestStateFailed {
			e.t.Fatalf("agent could not provide redacted tail: %v", out.Error)
		}
		if out.State != gen.LogRequestStateCompleted || out.Text == nil {
			return false
		}
		if out.Generation != request.Generation || out.EnvironmentID != id || len(*out.Text) == 0 || len(*out.Text) > 16<<10 {
			e.t.Fatal("log tail is empty, oversized, or not scoped to the requested generation")
		}
		return true
	})
}

// Exercise data preservation through the preview's application API. An expiry
// edit must not replay the baseline or advance the deployment/reset fencing.
func (e *live) extendWithoutReset(env gen.Environment) {
	ctx := context.Background()
	var pe v1alpha1.PreviewEnvironment
	e.must(e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: env.Name}, &pe))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	e.must(err)
	port := listener.Addr().(*net.TCPAddr).Port
	e.must(listener.Close())
	forwardCtx, cancel := context.WithCancel(ctx)
	forward := exec.CommandContext(forwardCtx, "kubectl", "--context", e.context, "-n", pe.Status.Namespace, "port-forward", "service/api", fmt.Sprintf("%d:8080", port), "--address=127.0.0.1")
	forward.Stdout, forward.Stderr = io.Discard, io.Discard
	e.must(forward.Start())
	defer func() { cancel(); _ = forward.Wait() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	e.eventually("preview application port forward", 30*time.Second, func() bool {
		response, err := httpClient.Get(base + "/health")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	var products []struct {
		SKU string `json:"sku"`
	}
	e.previewRequest(httpClient, base, "GET", "/api/products", nil, 200, &products)
	if len(products) == 0 {
		e.t.Fatal("baseline did not create ShopFlow products")
	}
	var created struct {
		ID int64 `json:"id"`
	}
	e.previewRequest(httpClient, base, "POST", "/api/orders", map[string]any{"sku": products[0].SKU, "quantity": 1, "email": "p5-preserved@example.invalid"}, 201, &created)
	extended := e.action(env, "extend")
	if extended.Generation != env.Generation || extended.ResetNonce != env.ResetNonce {
		e.t.Fatal("extending expiry changed the deployment or reset generation")
	}
	e.eventually("extended expiry reached the live agent", 30*time.Second, func() bool {
		if e.k.Get(ctx, types.NamespacedName{Namespace: "heimdall-system", Name: env.Name}, &pe) != nil {
			return false
		}
		return pe.Spec.ExpiresAt.Unix() == extended.ExpiresAt.Unix() && pe.Status.ObservedGeneration == pe.Generation
	})
	var orders []struct {
		ID int64 `json:"id"`
	}
	e.previewRequest(httpClient, base, "GET", "/api/orders", nil, 200, &orders)
	for _, order := range orders {
		if order.ID == created.ID {
			e.t.Log("API expiry extension preserved deployment generation and existing application data")
			return
		}
	}
	e.t.Fatal("expiry extension erased the existing ShopFlow order")
}

func (e *live) previewRequest(c *http.Client, base, method, path string, input any, status int, out any) {
	e.t.Helper()
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		e.must(err)
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequest(method, base+path, body)
	e.must(err)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := c.Do(r)
	e.must(err)
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	e.must(err)
	if response.StatusCode != status {
		e.t.Fatalf("preview %s %s returned %d, want %d: %s", method, path, response.StatusCode, status, raw)
	}
	if out != nil {
		e.must(json.Unmarshal(raw, out))
	}
}

func (e *live) request(method, path string, input any, nonce string, status int, out any) {
	e.t.Helper()
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		e.must(err)
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequest(method, e.apiURL+path, body)
	e.must(err)
	r.Header.Set("Authorization", "Bearer "+e.user)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if nonce != "" {
		r.Header.Set("Idempotency-Key", nonce)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(r)
	e.must(err)
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	e.must(err)
	if response.StatusCode != status {
		e.t.Fatalf("%s %s: status %d want %d: %s", method, path, response.StatusCode, status, raw)
	}
	if out != nil {
		e.must(json.Unmarshal(raw, out))
	}
}
func (e *live) eventually(what string, within time.Duration, fn func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Second)
	}
	e.diagnostics()
	e.t.Fatal("timed out: " + what)
}
func (e *live) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}
func (e *live) command(name string, args ...string) {
	e.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("%s failed: %v\n%s", name, err, out)
	}
	if len(out) > 0 {
		e.t.Logf("%s", out)
	}
}
func (e *live) diagnostics() {
	for _, args := range [][]string{{"--context", e.context, "-n", "heimdall-system", "get", "pods,previewenvironments", "-o", "wide"}, {"--context", e.context, "-n", "heimdall-system", "logs", "deployment/heimdall-agent", "--all-containers", "--tail=60"}, {"--context", e.context, "get", "pods", "-A", "-o", "wide"}, {"--context", e.context, "get", "events", "-A", "--sort-by=.metadata.creationTimestamp"}} {
		out, _ := exec.Command("kubectl", args...).CombinedOutput()
		e.t.Logf("%s", out)
	}
}
