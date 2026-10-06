//go:build !windows

// envtest does not build on Windows in controller-runtime v0.25; run on Linux
// or macOS (make envtest runs it in a container on Windows).

package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("source: envtest skipped; set KUBEBUILDER_ASSETS (make envtest)")
		os.Exit(m.Run())
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../charts/heimdall-agent/crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("envtest:", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	k8s, _ = client.New(cfg, client.Options{Scheme: scheme})
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// fixed is a Source returning whatever it is set to.
type fixed struct {
	envs []Environment
	err  error
}

func (f *fixed) Name() string  { return "test" }
func (f *fixed) Managed() bool { return true }
func (f *fixed) Snapshot(context.Context) (*Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &Snapshot{Environments: f.envs}, nil
}

func env(name string, pr, generation int64) Environment {
	return Environment{Name: name, Spec: v1alpha1.PreviewEnvironmentSpec{
		Tenant: "acme", Repository: "acme/demo", PullRequest: pr, Commit: strings.Repeat("a", 40), Generation: generation,
		EnvironmentID: name, Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now().Truncate(time.Second)),
		Config: v1alpha1.ConfigSource{Inline: fmt.Sprintf("version: 1 # generation %d\n", generation), SHA256: strings.Repeat("0", 64)},
	}}
}

func TestSyncerProjectsTheSource(t *testing.T) {
	if k8s == nil {
		t.Skip("requires envtest")
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "agent-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	manual := &v1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "applied-by-hand", Namespace: ns.Name}, Spec: env("applied-by-hand", 99, 1).Spec}
	if err := k8s.Create(ctx, manual); err != nil {
		t.Fatal(err)
	}
	src := &fixed{envs: []Environment{env("pr1", 1, 1), env("pr2", 2, 1)}}
	s := &Syncer{Source: src, Client: k8s, Namespace: ns.Name, Log: logr.Discard()}
	names := func() []string {
		var list v1alpha1.PreviewEnvironmentList
		if err := k8s.List(ctx, &list, client.InNamespace(ns.Name)); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, pe := range list.Items {
			out = append(out, pe.Name)
		}
		return out
	}

	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(), ","); got != "applied-by-hand,pr1,pr2" {
		t.Fatalf("objects: %s", got)
	}
	pe := &v1alpha1.PreviewEnvironment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pr1"}, pe); err != nil || pe.Labels[LabelSource] != "test" {
		t.Fatalf("pr1: %+v %v", pe.Labels, err)
	}

	// A new generation in the source updates the object in place.
	src.envs[0] = env("pr1", 1, 2)
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pr1"}, pe); err != nil || pe.Spec.Generation != 2 {
		t.Fatalf("pr1 not updated: %d %v", pe.Spec.Generation, err)
	}

	// The source becomes unavailable: nothing may be deleted.
	src.err = errors.New("control plane unreachable")
	if err := s.Sync(ctx); err == nil {
		t.Fatal("an unavailable source must be reported")
	}
	if got := strings.Join(names(), ","); got != "applied-by-hand,pr1,pr2" {
		t.Fatalf("objects changed while the source was down: %s", got)
	}

	// An authoritative snapshot without pr2 deletes it, and only it: objects
	// the source did not write are not its to delete.
	src.err, src.envs = nil, []Environment{env("pr1", 1, 2)}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(), ","); got != "applied-by-hand,pr1" {
		t.Fatalf("objects after removal: %s", got)
	}
}
