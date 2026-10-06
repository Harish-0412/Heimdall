package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func TestRealBreakGlass(t *testing.T) {
	selected := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if selected == "" {
		t.Skip("requires dedicated kind context")
	}
	if !strings.HasPrefix(selected, "kind-") {
		t.Fatal("only kind is allowed")
	}
	rc, _, err := clusterConfig("", selected, []string{selected})
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := clusterIdentity(rc)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../test/e2e/engine/heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, diags := config.Load(bytes.NewReader(raw), config.DefaultPolicy())
	if cfg == nil {
		t.Fatal(diags)
	}
	imageData, err := os.ReadFile(os.Getenv("HEIMDALL_E2E_IMAGES"))
	if err != nil {
		t.Fatal(err)
	}
	var images map[string]string
	if err = json.Unmarshal(imageData, &images); err != nil {
		t.Fatal(err)
	}
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	spec := engine.Spec{Config: cfg, Context: render.Context{Tenant: "p2", Repo: "local/shopflow", PR: 203, SHA: strings.TrimSpace(string(sha)), Generation: 1, EnvironmentID: "p2-breakglass", Owner: "local", ExpiresAt: time.Now().Add(time.Hour), URLSuffix: "bgls", Images: images, Policy: config.DefaultPolicy(), Platform: render.Platform{BaseDomain: "localtest.me", URLScheme: "http"}}}
	client, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(&engine.Kubernetes{Client: client}, 2*time.Second, nil)
	ns, scope, err := e.Scope(spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := render.Render(cfg, spec.Context)
	if err != nil {
		t.Fatal(err)
	}
	namespaces := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	cms := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	ctx := context.Background()
	for _, o := range p.Objects() {
		if o.GetObjectKind().GroupVersionKind().Kind == "Namespace" {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Resource(namespaces).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	hold := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "heimdall-e2e-hold", "namespace": ns}}}
	hold.SetLabels(scope)
	hold.SetFinalizers([]string{"heimdall.dev/e2e-hold"})
	if _, err = client.Resource(cms).Namespace(ns).Create(ctx, hold, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		o, err := client.Resource(cms).Namespace(ns).Get(ctx, hold.GetName(), metav1.GetOptions{})
		if err == nil {
			o.SetFinalizers(nil)
			_, _ = client.Resource(cms).Namespace(ns).Update(ctx, o, metav1.UpdateOptions{})
		}
		_ = client.Resource(namespaces).Delete(ctx, ns, metav1.DeleteOptions{})
	})
	state := filepath.Join(t.TempDir(), "state.json")
	if err = saveLocal(state, localSpec{Version: 1, KubeContext: selected, Cluster: cluster, Config: string(raw), Context: spec.Context}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run([]string{"down", "--state", state, "--allow-context", selected, "--timeout", "2s"}, &out, &stderr)
	if code == ExitOK || !strings.Contains(stderr.String(), "engine.destroy_stuck") {
		t.Fatalf("normal down did not preserve finalizers: %d %s", code, stderr.String())
	}
	status, err := e.Status(ctx, spec)
	if err != nil || status.Phase != "destroying" {
		t.Fatalf("terminating status: %v %v", status, err)
	}
	missingDir := filepath.Join(t.TempDir(), "missing", "evidence.json")
	if err = forceCleanup(ctx, rc, e, spec, "verify evidence-before-mutation", missingDir, 30*time.Second); err == nil {
		t.Fatal("missing evidence destination was accepted")
	}
	existing, err := client.Resource(cms).Namespace(ns).Get(ctx, hold.GetName(), metav1.GetOptions{})
	if err != nil || len(existing.GetFinalizers()) != 1 {
		t.Fatalf("finalizers changed without evidence: %v", err)
	}
	evidence := filepath.Join(t.TempDir(), "evidence.json")
	err = forceCleanup(ctx, rc, e, spec, "release the live test's deliberate finalizer", evidence, 30*time.Second)
	if err != nil && !strings.Contains(err.Error(), "engine.cleanup_pending") {
		t.Fatal(err)
	}
	document, err := os.ReadFile(evidence)
	if err != nil || !json.Valid(document) {
		t.Fatalf("invalid evidence: %v", err)
	}
	if !bytes.Contains(document, []byte("heimdall.dev/e2e-hold")) {
		t.Fatal("original finalizer evidence was lost")
	}
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err = engine.New(&engine.Kubernetes{Client: client}, time.Minute, nil).Destroy(wait, spec); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(namespaces).Get(ctx, ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace remains: %v", err)
	}
}
