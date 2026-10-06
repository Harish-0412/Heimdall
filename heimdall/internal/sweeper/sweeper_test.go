package sweeper

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/render"
	"github.com/heimdall-dev/heimdall/internal/source"
)

var start = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

const grace = 30 * time.Minute

// switchSource is available or not, on demand.
type switchSource struct {
	envs []source.Environment
	down bool
}

func (s *switchSource) Name() string  { return "test" }
func (s *switchSource) Managed() bool { return true }
func (s *switchSource) Snapshot(context.Context) (*source.Snapshot, error) {
	if s.down {
		return nil, errors.New("control plane unreachable")
	}
	return &source.Snapshot{Environments: s.envs}, nil
}

func previewNS(name, uid string, age time.Duration) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID(uid), CreationTimestamp: metav1.NewTime(start.Add(-age)),
		Labels: map[string]string{
			render.LabelPreview: "true", "app.kubernetes.io/managed-by": render.ManagedBy,
			render.LabelTenant: "acme", render.LabelRepo: "acme.demo", render.LabelPR: "9", render.LabelEnv: "env-9",
		},
	}}
}

type fixture struct {
	sweeper *Sweeper
	clock   *clocktesting.FakeClock
	source  *switchSource
	deleted []string
}

func newFixture(t *testing.T, objects ...client.Object) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	f := &fixture{clock: clocktesting.NewFakeClock(start), source: &switchSource{}}
	f.sweeper = &Sweeper{
		Source: f.source, Reader: c, Namespace: "heimdall-system", Grace: grace, Interval: time.Minute,
		Clock: f.clock, Log: logr.Discard(),
		Destroy: func(_ context.Context, ns string) error { f.deleted = append(f.deleted, ns); return nil },
	}
	return f
}

// pass advances the clock by one interval and sweeps.
func (f *fixture) pass() Report {
	f.clock.Step(time.Minute)
	return f.sweeper.Pass(context.Background())
}

func TestOrphanNeedsASecondPass(t *testing.T) {
	f := newFixture(t, previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour))
	if r := f.pass(); len(r.Deleted) > 0 || !slices.Equal(r.Suspected, []string{"heimdall-pr9-demo-zzzz"}) {
		t.Fatalf("first pass must only suspect: %+v", r)
	}
	if r := f.pass(); !slices.Equal(r.Deleted, []string{"heimdall-pr9-demo-zzzz"}) {
		t.Fatalf("second pass must delete: %+v", r)
	}
}

func TestYoungOrphanWaitsForTheGracePeriod(t *testing.T) {
	f := newFixture(t, previewNS("heimdall-pr9-demo-zzzz", "u1", 0))
	f.pass()
	for range 25 {
		if r := f.pass(); len(r.Deleted) > 0 {
			t.Fatalf("deleted at age %s, before the %s grace period", f.clock.Now().Sub(start), grace)
		}
	}
	f.clock.Step(10 * time.Minute)
	if r := f.pass(); len(r.Deleted) != 1 {
		t.Fatalf("not deleted after the grace period: %+v", r)
	}
}

func TestUnavailableSourceDeletesNothing(t *testing.T) {
	f := newFixture(t, previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour))
	f.pass() // suspected while the source was up
	f.source.down = true
	for range 10 {
		if r := f.pass(); r.SourceAvailable || len(r.Deleted) > 0 {
			t.Fatalf("deleted while the source was down: %+v", r)
		}
	}
	if len(f.deleted) != 0 {
		t.Fatal("Destroy was called while the source was down")
	}
	f.source.down = false
	if r := f.pass(); len(r.Deleted) != 1 {
		t.Fatalf("an authoritative pass after recovery should delete: %+v", r)
	}
}

func TestObjectListFailureDeletesNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.PreviewEnvironmentList); ok {
				return errors.New("forbidden")
			}
			return cl.List(ctx, list, opts...)
		}}).Build()
	f := newFixture(t)
	f.sweeper.Reader = c
	for range 5 {
		if r := f.pass(); r.SourceAvailable || len(r.Suspected)+len(r.Deleted) > 0 {
			t.Fatalf("acted without an authoritative object list: %+v", r)
		}
	}
}

func TestDesiredAndObjectBackedNamespacesAreKept(t *testing.T) {
	inSource := previewNS(render.NamespaceFor("acme/demo", 9, "zzzz"), "u1", 2*time.Hour)
	withObject := previewNS(render.NamespaceFor("acme/demo", 10, "yyyy"), "u2", 2*time.Hour)
	pe := &v1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "pr10", Namespace: "heimdall-system"},
		Spec: v1alpha1.PreviewEnvironmentSpec{Repository: "acme/demo", PullRequest: 10, URLSuffix: "yyyy"}}
	f := newFixture(t, inSource, withObject, pe)
	f.source.envs = []source.Environment{{Name: "pr9", Spec: v1alpha1.PreviewEnvironmentSpec{Repository: "acme/demo", PullRequest: 9, URLSuffix: "zzzz"}}}
	for range 5 {
		if r := f.pass(); len(r.Suspected)+len(r.Deleted) > 0 {
			t.Fatalf("touched a namespace that desired state or an object accounts for: %+v", r)
		}
	}
}

func TestOnlyOwnedNamespacesAreCandidates(t *testing.T) {
	noIdentity := previewNS("heimdall-pr9-demo-aaaa", "u1", 2*time.Hour)
	delete(noIdentity.Labels, render.LabelEnv)
	wrongPrefix := previewNS("team-pr9-demo-bbbb", "u2", 2*time.Hour)
	foreign := previewNS("heimdall-pr9-demo-cccc", "u3", 2*time.Hour)
	foreign.Labels["app.kubernetes.io/managed-by"] = "someone-else"
	f := newFixture(t, noIdentity, wrongPrefix, foreign)
	for range 5 {
		if r := f.pass(); len(r.Suspected)+len(r.Deleted) > 0 {
			t.Fatalf("unowned namespace became a candidate: %+v", r)
		}
	}
}

func TestRecreatedNamespaceRestartsConfirmation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour)).Build()
	f := newFixture(t)
	f.sweeper.Reader = c
	f.pass()
	// Same name, new object: the earlier sighting does not count for it.
	if err := c.Delete(context.Background(), previewNS("heimdall-pr9-demo-zzzz", "u1", 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), previewNS("heimdall-pr9-demo-zzzz", "u2", 2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := f.pass(); len(r.Deleted) > 0 || len(r.Suspected) != 1 {
		t.Fatalf("a new namespace must be confirmed afresh: %+v", r)
	}
}

func TestDryRunOnlyReports(t *testing.T) {
	f := newFixture(t, previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour))
	f.sweeper.DryRun = true
	f.pass()
	if r := f.pass(); !slices.Equal(r.WouldDelete, []string{"heimdall-pr9-demo-zzzz"}) || len(f.deleted) > 0 {
		t.Fatalf("dry run: %+v, deleted %v", r, f.deleted)
	}
}

func TestFailedDeletionIsRetried(t *testing.T) {
	f := newFixture(t, previewNS("heimdall-pr9-demo-zzzz", "u1", 2*time.Hour))
	fail := true
	f.sweeper.Destroy = func(context.Context, string) error {
		if fail {
			fail = false
			return errors.New("engine.busy")
		}
		f.deleted = append(f.deleted, "x")
		return nil
	}
	f.pass()
	if r := f.pass(); len(r.Failed) != 1 {
		t.Fatalf("want a failure: %+v", r)
	}
	if r := f.pass(); len(r.Deleted) != 1 {
		t.Fatalf("want a retry: %+v", r)
	}
}

func TestSuspectThatReappearsInDesiredStateIsForgotten(t *testing.T) {
	ns := previewNS(render.NamespaceFor("acme/demo", 9, "zzzz"), "u1", 2*time.Hour)
	f := newFixture(t, ns)
	f.pass()
	f.source.envs = []source.Environment{{Name: "pr9", Spec: v1alpha1.PreviewEnvironmentSpec{Repository: "acme/demo", PullRequest: 9, URLSuffix: "zzzz"}}}
	f.pass()
	f.source.envs = nil
	if r := f.pass(); len(r.Deleted) > 0 {
		t.Fatal("an earlier sighting survived the namespace becoming desired again")
	}
}
