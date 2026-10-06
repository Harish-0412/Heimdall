package source

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
)

type fakeControl struct {
	s   gen.DesiredSnapshot
	err error
}

func TestAPIPreparationFailureIsGenerationFencedAndClearsOnRecovery(t *testing.T) {
	ctx := context.Background()
	p := config.DefaultPolicy()
	p.AllowedSecrets, p.AllowedRegistries = []string{}, []string{"registry.test/previews/"}
	spec := v1.PreviewEnvironmentSpec{Tenant: "acme", EnvironmentID: "preview", Generation: 1, Config: v1.ConfigSource{Inline: "config", SHA256: bundle.ConfigDigest([]byte("config")), Bundle: "registry.test/previews/bundle@sha256:" + strings.Repeat("a", 64)}}
	b, _ := json.Marshal(spec)
	c := &fakeControl{s: gen.DesiredSnapshot{TenantID: "tenant", TenantSlug: "acme", Revision: "1", Policy: p, Environments: []gen.Environment{{Id: "preview", Name: "preview", TenantID: "tenant", ClusterID: "cluster", Generation: 1, DesiredState: gen.EnvironmentDesiredStateRunning, Spec: b}}}}
	a := &API{Control: c, ClusterID: "cluster", Interval: time.Second, LoadBundle: func(context.Context, string, string) (*bundle.Contents, error) {
		return nil, errors.New("private registry detail must stay local")
	}}
	snap, err := a.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prepare(ctx, &snap.Environments[0]); err == nil {
		t.Fatal("unavailable artifact accepted")
	}
	failure := a.PreparationFailures()["preview"]
	if failure.Generation != 1 || failure.At.IsZero() {
		t.Fatal("first-generation failure has no fenced observation")
	}
	_ = a.Prepare(ctx, &snap.Environments[0])
	if a.PreparationFailures()["preview"] != failure {
		t.Fatal("retry changed the idempotent observation")
	}
	a.LoadBundle = func(context.Context, string, string) (*bundle.Contents, error) {
		return &bundle.Contents{Config: []byte("config")}, nil
	}
	if err := a.Prepare(ctx, &snap.Environments[0]); err != nil || len(a.PreparationFailures()) != 0 {
		t.Fatalf("recovery retained failure: %v", err)
	}
	a.LoadBundle = func(context.Context, string, string) (*bundle.Contents, error) { return nil, errors.New("offline") }
	_ = a.Prepare(ctx, &snap.Environments[0])
	c.s.Revision = "2"
	c.s.Environments[0].Generation = 2 // CI pending: committed spec is still generation 1.
	if _, err := a.Snapshot(ctx); err != nil || len(a.PreparationFailures()) != 0 {
		t.Fatalf("new generation inherited previous failure: %v", err)
	}
	_ = a.Prepare(ctx, &snap.Environments[0]) // Late completion from generation 1.
	if len(a.PreparationFailures()) != 0 {
		t.Fatal("late preparation resurrected a stale observation")
	}
}

func TestAPIFixtureOwnershipAndPruning(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	pe := &v1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: "preview", UID: "pe-uid", Labels: map[string]string{LabelSource: "api"}}, Spec: v1.PreviewEnvironmentSpec{EnvironmentID: "preview", Generation: 2, Data: &v1.DataSource{ConfigMap: "heimdall-data-preview-current"}}}
	fixture := func(name, env string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: name, Labels: map[string]string{LabelSource: "api", "heimdall.dev/env": env}}, Immutable: new(true)}
	}
	current, old, other := fixture(pe.Spec.Data.ConfigMap, "preview"), fixture("heimdall-data-preview-old", "preview"), fixture("heimdall-data-other-current", "other")
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pe, current, old, other).WithStatusSubresource(pe).Build()
	a := &API{Client: k, Namespace: "agent"}
	env := Environment{Name: pe.Name, Spec: pe.Spec}
	if err := a.AfterApply(ctx, env); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{current.Name, old.Name} {
		var cm corev1.ConfigMap
		if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: name}, &cm); err != nil || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != pe.UID {
			t.Fatalf("fixture %s was not bound to its exact CR UID: %v", name, err)
		}
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: pe.Name}, pe); err != nil {
		t.Fatal(err)
	}
	pe.Status.Phase, pe.Status.DeployedGeneration = v1.PhaseReady, 1
	if err := k.Status().Update(ctx, pe); err != nil {
		t.Fatal(err)
	}
	if err := a.AfterApply(ctx, env); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: old.Name}, old); err != nil {
		t.Fatal("previous fixture pruned before the new generation converged")
	}
	pe.Status.DeployedGeneration = 2
	if err := k.Status().Update(ctx, pe); err != nil {
		t.Fatal(err)
	}
	if err := a.AfterApply(ctx, env); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: old.Name}, old); !apierrors.IsNotFound(err) {
		t.Fatal("old fixture retained after successful convergence")
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: other.Name}, other); err != nil || len(other.OwnerReferences) != 0 {
		t.Fatal("one PR changed another PR's fixture")
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: current.Name}, current); err != nil {
		t.Fatal(err)
	}
	current.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PreviewEnvironment", Name: "another", UID: "another-uid"}}
	if err := k.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	pe.Spec.DesiredState = v1.DesiredDestroyed
	if err := k.Update(ctx, pe); err != nil {
		t.Fatal(err)
	}
	env.Spec = pe.Spec
	if err := a.AfterApply(ctx, env); err == nil {
		t.Fatal("cleanup accepted a fixture belonging to another UID")
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: current.Name}, current); err != nil {
		t.Fatal("cleanup deleted another object's fixture")
	}
	current.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PreviewEnvironment", Name: pe.Name, UID: pe.UID}}
	if err := k.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	env.Spec.Generation++
	if err := a.AfterApply(ctx, env); err == nil {
		t.Fatal("fixture deletion accepted an unobserved projection generation")
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: current.Name}, current); err != nil {
		t.Fatal("stale projection deleted the current fixture")
	}
	env.Spec.Generation--
	if err := a.AfterApply(ctx, env); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: current.Name}, current); !apierrors.IsNotFound(err) {
		t.Fatal("explicit destroy retained its own fixture")
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: other.Name}, other); err != nil {
		t.Fatal("explicit destroy deleted another PR's fixture")
	}
}

func TestAPIPrunesOnlyOldUnreferencedOwnerlessFixtures(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	fixture := func(name string, age time.Duration) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: "heimdall-data-preview-" + name, CreationTimestamp: metav1.NewTime(time.Now().Add(-age)), Labels: map[string]string{LabelSource: "api", "heimdall.dev/env": "preview"}}, Immutable: new(true)}
	}
	orphan, young, used, owned := fixture("orphan", 2*time.Hour), fixture("young", time.Minute), fixture("used", 2*time.Hour), fixture("owned", 2*time.Hour)
	owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PreviewEnvironment", Name: "another", UID: "another-uid"}}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan, young, used, owned).Build()
	b, _ := json.Marshal(v1.PreviewEnvironmentSpec{Data: &v1.DataSource{ConfigMap: used.Name}})
	a := &API{Client: k, Namespace: "agent", records: map[string]gen.Environment{"preview": {Spec: b}}}
	if err := a.pruneUnownedData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: orphan.Name}, orphan); !apierrors.IsNotFound(err) {
		t.Fatal("old unreferenced ownerless fixture retained")
	}
	for _, cm := range []*corev1.ConfigMap{young, used, owned} {
		if err := k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: cm.Name}, cm); err != nil {
			t.Fatalf("unsafe fixture deletion %s: %v", cm.Name, err)
		}
	}
}

func (f *fakeControl) Desired(context.Context) (gen.DesiredSnapshot, error) { return f.s, f.err }

func TestAPISnapshotPreservesCommittedGenerationAndFailsClosed(t *testing.T) {
	policy := config.DefaultPolicy()
	policy.AllowedSecrets = []string{}
	policy.AllowedRegistries = []string{"registry.test/previews/"}
	spec := v1.PreviewEnvironmentSpec{Tenant: "acme", EnvironmentID: "preview", Generation: 2}
	b, _ := json.Marshal(spec)
	c := &fakeControl{s: gen.DesiredSnapshot{TenantID: "tenant", TenantSlug: "acme", Revision: "4", Policy: policy, Environments: []gen.Environment{{Id: "preview", Name: "preview", TenantID: "tenant", ClusterID: "cluster", Generation: 3, Spec: b}, {Id: "new", Name: "new", TenantID: "tenant", ClusterID: "cluster", Generation: 1, Spec: json.RawMessage(`{}`)}}}}
	a := &API{Control: c, ClusterID: "cluster", Interval: time.Second}
	snap, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Environments) != 1 || snap.Environments[0].Spec.Generation != 2 {
		t.Fatal("pending build overwrote committed generation")
	}
	if _, err = a.PolicyFor(context.Background(), "another"); err == nil {
		t.Fatal("policy crossed tenant")
	}
	c.err = errors.New("API unavailable")
	if snap, err = a.Snapshot(context.Background()); err == nil || snap != nil {
		t.Fatal("outage became authoritative empty")
	}
	c.err = nil
	c.s.Revision = "3"
	if _, err = a.Snapshot(context.Background()); err == nil {
		t.Fatal("revision regressed")
	}
	c.s.Revision = "5"
	c.s.Environments[0].ClusterID = "other"
	if _, err = a.Snapshot(context.Background()); err == nil {
		t.Fatal("cross-cluster projection accepted")
	}
}

func TestAPIMaterializesOnlyApprovedExactBytesInAgentNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	seed := []byte("-- sanitised fixture\n")
	sha := bundle.ConfigDigest(seed)
	cfg := "approvedconfig"
	pe := Environment{Spec: v1.PreviewEnvironmentSpec{Tenant: "acme", EnvironmentID: "preview", Generation: 1, Config: v1.ConfigSource{Inline: cfg, SHA256: bundle.ConfigDigest([]byte(cfg)), Bundle: "registry.test/previews/bundle@sha256:" + strings.Repeat("a", 64)}, Data: &v1.DataSource{ConfigMap: "heimdall-data-preview-" + sha[:12], Key: "seed.sql", Approval: v1.DataApproval{SHA256: sha, Sanitised: true, ApprovedBy: "operator", Reason: "sanitised offline"}}}}
	p := config.DefaultPolicy()
	p.AllowedRegistries = []string{"registry.test/previews/"}
	a := &API{Client: k, Namespace: "agent", Interval: time.Second, tenant: "acme", policy: p, policyAt: time.Now(), LoadBundle: func(context.Context, string, string) (*bundle.Contents, error) {
		return &bundle.Contents{Config: []byte(cfg), Seed: seed}, nil
	}}
	if err := a.Prepare(context.Background(), &pe); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if k.Get(context.Background(), types.NamespacedName{Namespace: "agent", Name: pe.Spec.Data.ConfigMap}, &cm) != nil || cm.Immutable == nil || !*cm.Immutable || cm.Data["seed.sql"] != string(seed) {
		t.Fatal("approved fixture not immutable and scoped")
	}
	seed = []byte("-- tampered\n")
	if err := a.Prepare(context.Background(), &pe); err == nil {
		t.Fatal("tampered fixture accepted")
	}
	seed = []byte("-- sanitised fixture\n")
	pe.Spec.Data.ConfigMap = "operator-secret"
	if err := a.Prepare(context.Background(), &pe); err == nil {
		t.Fatal("arbitrary cluster object overwrite accepted")
	}
	pe.Spec.DesiredState = v1.DesiredDestroyed
	a.LoadBundle = func(context.Context, string, string) (*bundle.Contents, error) {
		t.Fatal("destroy depended on registry availability")
		return nil, nil
	}
	if err := a.Prepare(context.Background(), &pe); err != nil {
		t.Fatal(err)
	}
}
