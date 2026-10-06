package source

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
)

// FieldManager owns the spec of PreviewEnvironments written from a source.
const FieldManager = "heimdall-agent-source"

// Syncer projects a managed source onto PreviewEnvironment objects: one per
// desired environment, created or updated with server-side apply; objects
// the source no longer lists (and that carry its label) are deleted, which
// destroys their environments through the finalizer. When the source cannot
// be read it changes nothing.
type Syncer struct {
	Source    Source
	Client    client.Client
	Namespace string
	Interval  time.Duration
	Metrics   *Metrics
	Log       logr.Logger
}

// Start implements manager.Runnable.
func (s *Syncer) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error(err, "desired-state sync incomplete; nothing was deleted", "source", s.Source.Name())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (s *Syncer) NeedLeaderElection() bool { return true }

// Sync performs one pass.
func (s *Syncer) Sync(ctx context.Context) error {
	snap, err := s.Source.Snapshot(ctx)
	s.Metrics.Observe(snap, err)
	if err != nil {
		return fmt.Errorf("source unavailable: %w", err)
	}
	desired := map[string]bool{}
	var errs []error
	for _, env := range snap.Environments {
		desired[env.Name] = true
		u, err := s.object(env)
		if err == nil {
			err = s.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("apply %s: %w", env.Name, err))
		}
	}
	var list v1alpha1.PreviewEnvironmentList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace), client.MatchingLabels{LabelSource: s.Source.Name()}); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range list.Items {
		pe := &list.Items[i]
		if desired[pe.Name] || !pe.DeletionTimestamp.IsZero() {
			continue
		}
		s.Log.Info("environment removed from desired state; deleting", "environment", pe.Name, "revision", snap.Revision)
		uid := pe.UID
		if err := s.Client.Delete(ctx, pe, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", pe.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Syncer) object(env Environment) (*unstructured.Unstructured, error) {
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&env.Spec)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("PreviewEnvironment")
	u.SetName(env.Name)
	u.SetNamespace(s.Namespace)
	u.SetLabels(map[string]string{LabelSource: s.Source.Name()})
	return u, nil
}

// Metrics describe source health.
type Metrics struct {
	up           prometheus.Gauge
	environments prometheus.Gauge
	failures     prometheus.Counter
}

// NewMetrics registers source metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		up: prometheus.NewGauge(prometheus.GaugeOpts{Name: "heimdall_agent_source_up",
			Help: "1 when the last desired-state read was authoritative, else 0."}),
		environments: prometheus.NewGauge(prometheus.GaugeOpts{Name: "heimdall_agent_source_environments",
			Help: "Environments in the last authoritative desired state."}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{Name: "heimdall_agent_source_failures_total",
			Help: "Desired-state reads that failed (nothing is deleted after one)."}),
	}
	reg.MustRegister(m.up, m.environments, m.failures)
	return m
}

// Observe records a read. A nil *Metrics records nothing.
func (m *Metrics) Observe(s *Snapshot, err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.up.Set(0)
		m.failures.Inc()
		return
	}
	m.up.Set(1)
	m.environments.Set(float64(len(s.Environments)))
}
