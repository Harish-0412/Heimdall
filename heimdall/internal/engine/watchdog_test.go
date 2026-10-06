package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// waitStub is a Cluster whose Wait never succeeds on its own (unless ready
// is closed) and whose pods are fixed.
type waitStub struct {
	Cluster
	pods     []corev1.Pod
	ready    chan struct{}
	mu       sync.Mutex
	selector string
}

func (w *waitStub) Wait(ctx context.Context, _ Resource, _ *unstructured.Unstructured, _ bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ready:
		return nil
	}
}

func (w *waitStub) List(_ context.Context, _ Resource, _ string, selector string) ([]unstructured.Unstructured, error) {
	w.mu.Lock()
	w.selector = selector
	w.mu.Unlock()
	var out []unstructured.Unstructured
	for i := range w.pods {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&w.pods[i])
		if err != nil {
			return nil, err
		}
		out = append(out, unstructured.Unstructured{Object: m})
	}
	return out, nil
}

func deployment() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "p"},
		"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "api"}}}}}
}

func crashLooping(restarts int32) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute))},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: restarts,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}}}
}

func TestAwaitStopsOnAPermanentFailure(t *testing.T) {
	stub := &waitStub{pods: []corev1.Pod{crashLooping(4)}, ready: make(chan struct{})}
	e := &Engine{cluster: stub, timeout: time.Minute, watchdog: 10 * time.Millisecond}
	start := time.Now()
	err := e.await(context.Background(), deployments, deployment())
	if errorCode(err) != "engine.workload_failed" {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the watchdog did not stop the wait early")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.selector != "app.kubernetes.io/name=api" {
		t.Errorf("selector %q", stub.selector)
	}
	t.Log(err)
}

func TestAwaitIgnoresTransientFailures(t *testing.T) {
	stub := &waitStub{pods: []corev1.Pod{crashLooping(1)}, ready: make(chan struct{})}
	e := &Engine{cluster: stub, timeout: time.Minute, watchdog: 10 * time.Millisecond}
	time.AfterFunc(200*time.Millisecond, func() { close(stub.ready) })
	if err := e.await(context.Background(), deployments, deployment()); err != nil {
		t.Fatalf("a single restart stopped the wait: %v", err)
	}
}

func TestAwaitKeepsTheCallersCancellation(t *testing.T) {
	stub := &waitStub{ready: make(chan struct{})}
	e := &Engine{cluster: stub, timeout: time.Minute, watchdog: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.await(ctx, deployments, deployment()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestAwaitIgnoresAFailedPreviousRevision(t *testing.T) {
	old := crashLooping(4)
	old.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry.example/api@sha256:old"}}
	wanted := deployment()
	if err := unstructured.SetNestedField(wanted.Object, map[string]any{
		"containers": []any{map[string]any{"name": "app", "image": "registry.example/api@sha256:new"}},
	}, "spec", "template", "spec"); err != nil {
		t.Fatal(err)
	}
	stub := &waitStub{pods: []corev1.Pod{old}, ready: make(chan struct{})}
	e := &Engine{cluster: stub, timeout: time.Minute, watchdog: 10 * time.Millisecond}
	time.AfterFunc(100*time.Millisecond, func() { close(stub.ready) })
	if err := e.await(context.Background(), deployments, wanted); err != nil {
		t.Fatalf("old revision stopped the replacement: %v", err)
	}
	// The identical failure in the requested revision must still fail fast.
	old.Spec.Containers[0].Image = "registry.example/api@sha256:new"
	stub = &waitStub{pods: []corev1.Pod{old}, ready: make(chan struct{})}
	e.cluster = stub
	if err := e.await(context.Background(), deployments, wanted); errorCode(err) != "engine.workload_failed" {
		t.Fatalf("current revision was not blocked: %v", err)
	}
}

func TestMatchesTemplateAllowsPodDefaults(t *testing.T) {
	wanted := deployment()
	if err := unstructured.SetNestedField(wanted.Object, map[string]any{
		"containers": []any{map[string]any{"name": "app", "image": "registry.example/api@sha256:new"}},
	}, "spec", "template", "spec"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a", DNSPolicy: corev1.DNSClusterFirst,
		Containers: []corev1.Container{{Name: "app", Image: "registry.example/api@sha256:new", ImagePullPolicy: corev1.PullIfNotPresent}}}}
	if !matchesTemplate(wanted, pod) {
		t.Error("pod-only fields and defaults excluded the current pod")
	}
}

func TestAwaitIgnoresPreviousRevisionAfterClearingEnvironment(t *testing.T) {
	old := crashLooping(4)
	old.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry.example/api@sha256:same",
		Env: []corev1.EnvVar{{Name: "MODE", Value: "old-mode"}}}}
	wanted := deployment()
	if err := unstructured.SetNestedField(wanted.Object, map[string]any{
		"containers": []any{map[string]any{"name": "app", "image": "registry.example/api@sha256:same",
			"env": []any{map[string]any{"name": "MODE", "value": ""}}}},
	}, "spec", "template", "spec"); err != nil {
		t.Fatal(err)
	}
	stub := &waitStub{pods: []corev1.Pod{old}, ready: make(chan struct{})}
	e := &Engine{cluster: stub, timeout: time.Minute, watchdog: 10 * time.Millisecond}
	time.AfterFunc(100*time.Millisecond, func() { close(stub.ready) })
	if err := e.await(context.Background(), deployments, wanted); err != nil {
		t.Fatalf("old environment stopped the replacement: %v", err)
	}
	old.Spec.Containers[0].Env[0].Value = ""
	old.Spec.NodeName = "node-a"
	old.Spec.DNSPolicy = corev1.DNSClusterFirst
	old.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	e.cluster = &waitStub{pods: []corev1.Pod{old}, ready: make(chan struct{})}
	if err := e.await(context.Background(), deployments, wanted); errorCode(err) != "engine.workload_failed" {
		t.Fatalf("defaulted current revision was not blocked: %v", err)
	}
}
