//go:build !windows

// envtest does not build on Windows in controller-runtime v0.25; run these
// suites on Linux or macOS, or use the Docker command in docs/agent.md on Windows.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// The suite runs the controller against a real API server (envtest) with
// the generated CRD, and a scripted Operator in place of the engine: envtest
// has no kubelet, so real workloads would never become ready. The engine is
// covered against real clusters by its own tests and the kind e2e.

var (
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("controller: envtest skipped; set KUBEBUILDER_ASSETS (make envtest)")
		os.Exit(m.Run())
	}
	// Once, before anything logs: controller-runtime's logger is a promise
	// that must be fulfilled with a real sink, never with itself.
	logf.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../charts/heimdall-agent/crds"}, ErrorIfCRDPathMissing: true}
	var err error
	if restConfig, err = env.Start(); err != nil {
		fmt.Println("envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func needEnv(t *testing.T) {
	t.Helper()
	if restConfig == nil {
		t.Skip("requires envtest (KUBEBUILDER_ASSETS)")
	}
}

// call records one Operator invocation.
type call struct {
	op         string
	generation int64
	nonce      int64
}

// script is a controllable stand-in for the engine.
type script struct {
	mu      sync.Mutex
	calls   []call
	active  atomic.Int32 // operations running right now
	overlap atomic.Bool  // two operations ran concurrently for one environment

	apply   func(ctx context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error)
	reset   func(ctx context.Context, spec engine.Spec, nonce int64) (*engine.Result, error)
	destroy func(ctx context.Context, spec engine.Spec) (*engine.Result, error)
	status  func(spec engine.Spec) (*engine.Status, error)
	// diagnose, when set, is the reconciler's Diagnoser.
	diagnose func(spec engine.Spec, f diagnose.Failure) (*diagnose.Report, error)
}

type scriptDiagnoser struct{ s *script }

func (d scriptDiagnoser) Diagnose(_ context.Context, spec engine.Spec, f diagnose.Failure) (*diagnose.Report, error) {
	return d.s.diagnose(spec, f)
}

func (s *script) record(c call) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *script) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.op == op {
			n++
		}
	}
	return n
}

func (s *script) factory() OperatorFactory {
	return func(obs engine.Observer) Operator { return &fakeOperator{s: s, emit: obs} }
}

type fakeOperator struct {
	s    *script
	emit engine.Observer
}

func (f *fakeOperator) observe(ev engine.Event) {
	if f.emit != nil {
		f.emit(ev)
	}
}

func (f *fakeOperator) enter() func() {
	if f.s.active.Add(1) > 1 {
		f.s.overlap.Store(true)
	}
	return func() { f.s.active.Add(-1) }
}

func (f *fakeOperator) Prepare(_ context.Context, spec engine.Spec) (string, error) {
	return render.NamespaceFor(spec.Context.Repo, spec.Context.PR, spec.Context.URLSuffix), nil
}

// readyApply emits one step per stage and succeeds.
func readyApply(_ context.Context, spec engine.Spec, emit engine.Observer) (*engine.Result, error) {
	for _, step := range []string{"guardrails/setup", "dependencies/start", "application/wave-1", "smoke/run"} {
		emit(engine.Event{Operation: "apply", Stage: step, State: "running", Generation: spec.Context.Generation, At: time.Now()})
		emit(engine.Event{Operation: "apply", Stage: step, State: "succeeded", Generation: spec.Context.Generation, At: time.Now(), Duration: time.Millisecond})
	}
	return &engine.Result{Namespace: render.NamespaceFor(spec.Context.Repo, spec.Context.PR, spec.Context.URLSuffix),
		Generation: spec.Context.Generation, Phase: "ready",
		URLs: []render.URL{{Service: "app", Primary: true, URL: "https://pr7-demo-abcd.preview.example.com"}}}, nil
}

func (f *fakeOperator) Apply(ctx context.Context, spec engine.Spec) (*engine.Result, error) {
	defer f.enter()()
	f.s.record(call{"apply", spec.Context.Generation, 0})
	fn := f.s.apply
	if fn == nil {
		fn = readyApply
	}
	return fn(ctx, spec, f.observe)
}

func (f *fakeOperator) Reset(ctx context.Context, spec engine.Spec, nonce int64) (*engine.Result, error) {
	defer f.enter()()
	f.s.record(call{"reset", spec.Context.Generation, nonce})
	if f.s.reset != nil {
		return f.s.reset(ctx, spec, nonce)
	}
	return &engine.Result{Phase: "ready", Generation: spec.Context.Generation}, nil
}

func (f *fakeOperator) Destroy(ctx context.Context, spec engine.Spec) (*engine.Result, error) {
	defer f.enter()()
	f.s.record(call{"destroy", spec.Context.Generation, 0})
	if f.s.destroy != nil {
		return f.s.destroy(ctx, spec)
	}
	return &engine.Result{Phase: "destroyed", Generation: spec.Context.Generation}, nil
}

func (f *fakeOperator) Status(_ context.Context, spec engine.Spec) (*engine.Status, error) {
	if f.s.status != nil {
		return f.s.status(spec)
	}
	return &engine.Status{Result: engine.Result{Phase: "ready"}}, nil
}

// harness runs one manager with the reconciler in a fresh namespace.
type harness struct {
	t      *testing.T
	ns     string
	client client.Client
	script *script
	stop   context.CancelFunc
	done   chan struct{}
	guard  *switchGuard
}

type switchGuard struct{ ok atomic.Bool }

func (g *switchGuard) Allowed() bool { return g.ok.Load() }

func newHarness(t *testing.T, s *script) *harness {
	t.Helper()
	needEnv(t)
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "agent-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ns: ns.Name, client: c, script: s, guard: &switchGuard{}}
	h.guard.ok.Store(true)
	h.start()
	t.Cleanup(h.shutdown)
	return h
}

// start runs a manager, as an agent process would. Calling it again after
// shutdown simulates a restart.
func (h *harness) start() {
	h.t.Helper()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{h.ns: {}}},
		// Each test runs its own manager in this one process.
		Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	n := NewNotifier()
	r := &Reconciler{
		Client:      mgr.GetClient(),
		Specs:       SpecBuilder{Policy: config.DefaultPolicy(), Platform: render.Platform{BaseDomain: "preview.example.com"}},
		Runner:      NewRunner(h.script.factory(), 4, n.Notify),
		Guard:       h.guard,
		Resync:      200 * time.Millisecond,
		MaxBackoff:  2 * time.Second,
		BaseBackoff: 200 * time.Millisecond,
		GuardRetry:  200 * time.Millisecond,
	}
	if h.script.diagnose != nil {
		r.Diagnoser = scriptDiagnoser{h.script}
	}
	if err := r.SetupWithManager(mgr, n, 2); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.stop, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		if err := mgr.Start(ctx); err != nil {
			h.t.Error(err)
		}
	}()
}

func (h *harness) shutdown() {
	if h.stop != nil {
		h.stop()
		<-h.done
		h.stop = nil
	}
}

const testImage = "ghcr.io/acme/app@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func configDoc(env string) string {
	return "version: 1\nservices:\n  app:\n    image: " + testImage + "\n    port: 8080\n    public: true\n    health: {path: /h}\n" +
		"    env: {MODE: " + env + "}\n"
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (h *harness) environment(name string) *v1alpha1.PreviewEnvironment {
	doc := configDoc("one")
	return &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns},
		Spec: v1alpha1.PreviewEnvironmentSpec{
			Tenant: "acme", Repository: "acme/demo", PullRequest: 7, Commit: strings.Repeat("a", 40), Generation: 1,
			EnvironmentID: "env-" + name, Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now().Add(48 * time.Hour)),
			Config: v1alpha1.ConfigSource{Inline: doc, SHA256: digest(doc)},
		},
	}
}

func (h *harness) create(pe *v1alpha1.PreviewEnvironment) {
	h.t.Helper()
	if err := h.client.Create(context.Background(), pe); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(name string) *v1alpha1.PreviewEnvironment {
	h.t.Helper()
	pe := &v1alpha1.PreviewEnvironment{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: name}, pe); err != nil {
		h.t.Fatal(err)
	}
	return pe
}

// update applies fn to the latest object, retrying on conflicts.
func (h *harness) update(name string, fn func(*v1alpha1.PreviewEnvironment)) {
	h.t.Helper()
	for range 10 {
		pe := h.get(name)
		fn(pe)
		err := h.client.Update(context.Background(), pe)
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "modified") {
			h.t.Fatal(err)
		}
	}
	h.t.Fatal("update kept conflicting")
}

// eventually polls the named object until cond holds or the deadline passes.
func (h *harness) eventually(name, what string, timeout time.Duration, cond func(*v1alpha1.PreviewEnvironment) bool) *v1alpha1.PreviewEnvironment {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last *v1alpha1.PreviewEnvironment
	for time.Now().Before(deadline) {
		last = h.get(name)
		if cond(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; status: %+v", what, last.Status)
	return nil
}

// gone waits until the named object no longer exists.
func (h *harness) gone(name string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: name}, &v1alpha1.PreviewEnvironment{})
		if client.IgnoreNotFound(err) == nil && err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("%s still exists", name)
}

func ready(pe *v1alpha1.PreviewEnvironment) bool {
	return pe.Status.Phase == v1alpha1.PhaseReady && pe.Status.DeployedGeneration == pe.Spec.Generation
}
