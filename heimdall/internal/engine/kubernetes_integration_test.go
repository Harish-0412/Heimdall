package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRealAPIContracts(t *testing.T) {
	selected := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if selected == "" {
		t.Skip("requires dedicated kind API server; see test/e2e/engine/run.sh")
	}
	if !strings.HasPrefix(selected, "kind-") {
		t.Fatal("only kind is allowed")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, selected, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k := &Kubernetes{Client: client}
	e := New(k, 10*time.Second, nil)
	ctx := context.Background()
	ns := "heimdall-engine-contracts"
	scope := map[string]string{render.LabelTenant: "p2", render.LabelRepo: "local.contracts", render.LabelPR: "1", render.LabelEnv: "engine-contracts", managedBy: render.ManagedBy}
	namespace := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
	namespace.SetLabels(scope)
	namespace.SetAnnotations(map[string]string{render.AnnotationExpiresAt: time.Now().UTC().Format(time.RFC3339)})
	namespace, err = k.Create(ctx, namespaces, namespace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		wait, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		current, err := k.Get(wait, namespaces, "", ns)
		if err == nil {
			_ = e.deleteAndWait(wait, namespaces, current)
		}
	})
	// Reconcile a namespace created by this manager through SSA, including a
	// metadata update on the next invocation.
	applyNS := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
	applyNS.SetLabels(scope)
	applyNS.SetAnnotations(map[string]string{render.AnnotationExpiresAt: time.Now().UTC().Format(time.RFC3339)})
	if _, err = k.Apply(ctx, namespaces, applyNS); err != nil {
		t.Fatal(err)
	}
	applyNS.SetAnnotations(map[string]string{render.AnnotationExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if _, err = k.Apply(ctx, namespaces, applyNS); err != nil {
		t.Fatal(err)
	}
	work, s, err := e.acquire(ctx, ns, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = e.acquire(ctx, ns, scope); errorCode(err) != "engine.busy" {
		t.Fatalf("concurrent acquisition: %v", err)
	}
	if err = s.begin(work, 2, "accepted specification", "apply"); err != nil {
		t.Fatal(err)
	}
	if err = s.begin(work, 1, "accepted specification", "apply"); errorCode(err) != "engine.stale_generation" {
		t.Fatalf("stale work: %v", err)
	}
	if err = s.begin(work, 2, "changed specification", "apply"); errorCode(err) != "engine.generation_conflict" {
		t.Fatalf("changed generation: %v", err)
	}
	s.close()
	_, s, err = e.acquire(ctx, ns, scope)
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	object := func(name, gen string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": ns}}}
		m := map[string]string{}
		for k, v := range scope {
			m[k] = v
		}
		if gen != "" {
			m[render.LabelGeneration] = gen
		}
		o.SetLabels(m)
		return o
	}
	for name, gen := range map[string]string{"old": "1", "current": "2", "future": "3", "malformed": "invalid", "unversioned": ""} {
		if _, err = k.Apply(ctx, configmaps, object(name, gen)); err != nil {
			t.Fatal(err)
		}
	}
	// A different manager owns a live field. Engine SSA must not take it over.
	external := object("current", "2")
	external.Object["data"] = map[string]any{"observedNamespace": string(namespace.GetUID())}
	patch, err := json.Marshal(external)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(configmaps.GVR).Namespace(ns).Patch(ctx, "current", types.ApplyPatchType, patch, metav1.PatchOptions{FieldManager: "external-contract-test"}); err != nil {
		t.Fatal(err)
	}
	external.Object["data"] = map[string]any{"observedNamespace": namespace.GetResourceVersion()}
	if _, err = k.Apply(ctx, configmaps, external); !fieldConflict(err) || selfConflict(err) {
		t.Fatalf("external field ownership was not respected: %v", err)
	}
	foreign := object("foreign", "1")
	foreign.SetLabels(map[string]string{render.LabelEnv: "different"})
	if _, err = k.Create(ctx, configmaps, foreign); err != nil {
		t.Fatal(err)
	}
	// Use a namespace object in the desired plan for the ownership scope.
	renderedNS := namespace.DeepCopy()
	plan := &render.Plan{Namespace: ns, Stages: []render.Stage{{Name: render.StageGuardrails, Steps: []render.Step{{Objects: []render.Object{renderedNS}}}}}}
	if err = e.prune(ctx, plan, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = k.Get(ctx, configmaps, ns, "old"); !apierrors.IsNotFound(err) {
		t.Fatalf("old generation survived: %v", err)
	}
	for _, name := range []string{"current", "future", "malformed", "unversioned", "foreign", journalName} {
		if _, err = k.Get(ctx, configmaps, ns, name); err != nil {
			t.Fatalf("unsafe prune %s: %v", name, err)
		}
	}
	live, err := k.Get(ctx, configmaps, ns, "current")
	if err != nil {
		t.Fatal(err)
	}
	// An actual deletion watch deadline must surface, not report success.
	wait, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err = k.Wait(wait, configmaps, live, true)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watch deadline: %v", err)
	}
	if err = e.deleteAndWait(ctx, configmaps, live); err != nil {
		t.Fatal(err)
	}
	// Completion before watch registration must still succeed via initial list.
	if err = k.Wait(ctx, configmaps, live, true); err != nil {
		t.Fatal(err)
	}
	// Recover a crashed holder after its real journal lease expires. An old
	// holder's resourceVersion must not mutate the replacement's journal.
	_, previous, err := e.acquire(ctx, ns, scope)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := k.Get(ctx, configmaps, ns, journalName)
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeRecord(journal)
	if err != nil {
		t.Fatal(err)
	}
	r.LeaseUntil = time.Now().Add(-leaseDuration)
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(journal.Object, string(encoded), "data", "record"); err != nil {
		t.Fatal(err)
	}
	if _, err = k.Update(ctx, configmaps, journal); err != nil {
		t.Fatal(err)
	}
	_, replacement, err := e.acquire(ctx, ns, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = previous.event(ctx, Event{Operation: "apply", Stage: "stale", State: "running"}); errorCode(err) != "engine.lock_lost" {
		t.Fatalf("stale journal holder wrote state: %v", err)
	}
	previous.close()
	replacement.close()
}
