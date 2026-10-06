// Package sweeper removes orphaned preview namespaces: ones that no
// authoritative desired state accounts for, for example after an agent was
// replaced mid-destroy or an object was deleted without its finalizer.
//
// It is deliberately conservative (ADR 0005). A namespace is deleted only
// when every one of these holds in the same pass:
//
//  1. its ownership verifies: preview label, managed-by heimdall, the
//     heimdall- prefix, and tenant/repo/PR/environment identity labels;
//  2. it is older than the grace period;
//  3. an earlier pass already found the same namespace (same UID) orphaned;
//  4. the desired state, and the PreviewEnvironment objects, were both read
//     authoritatively in this pass.
//
// If either read fails, the pass deletes nothing. Dry-run only reports.
package sweeper

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/render"
	"github.com/heimdall-dev/heimdall/internal/source"
)

// Sweeper finds and removes orphaned preview namespaces.
type Sweeper struct {
	Source source.Source
	// Reader must read through the API server, not a cache: a stale read must
	// never be mistaken for an authoritative one.
	Reader client.Reader
	// Namespace holds the agent's PreviewEnvironment objects.
	Namespace string
	// Destroy removes one orphan (engine.DestroyOrphan, after granting the
	// agent access). It must refuse anything it does not own.
	Destroy  func(ctx context.Context, namespace string) error
	Grace    time.Duration
	Interval time.Duration
	DryRun   bool
	Clock    clock.PassiveClock
	Metrics  *Metrics
	Log      logr.Logger

	suspects map[string]suspect
}

type suspect struct {
	uid   types.UID
	first time.Time
}

// Report is the outcome of one pass.
type Report struct {
	SourceAvailable bool
	Suspected       []string // seen orphaned for the first time
	Deleted         []string
	WouldDelete     []string // dry run
	Failed          []string
}

// Start implements manager.Runnable.
func (s *Sweeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.Pass(ctx)
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// requiredLabels identify an environment's namespace.
var requiredLabels = []string{render.LabelTenant, render.LabelRepo, render.LabelPR, render.LabelEnv}

// Pass runs one sweep.
func (s *Sweeper) Pass(ctx context.Context) Report {
	if s.suspects == nil {
		s.suspects = map[string]suspect{}
	}
	if s.Clock == nil {
		s.Clock = clock.RealClock{}
	}
	var report Report
	desired, err := s.desired(ctx)
	if err != nil {
		s.Metrics.pass("source_unavailable")
		s.Log.Info("desired state unavailable; sweeping nothing", "error", err.Error())
		return report
	}
	report.SourceAvailable = true
	var list corev1.NamespaceList
	if err := s.Reader.List(ctx, &list, client.MatchingLabels{render.LabelPreview: "true", "app.kubernetes.io/managed-by": render.ManagedBy}); err != nil {
		s.Metrics.pass("list_failed")
		s.Log.Error(err, "cannot list preview namespaces; sweeping nothing")
		return report
	}
	s.Metrics.pass("ok")
	now := s.Clock.Now()
	orphaned := map[string]bool{}
	for i := range list.Items {
		ns := &list.Items[i]
		if !owned(ns) || ns.DeletionTimestamp != nil || desired[ns.Name] {
			continue
		}
		orphaned[ns.Name] = true
		prev, known := s.suspects[ns.Name]
		if !known || prev.uid != ns.UID {
			s.suspects[ns.Name] = suspect{uid: ns.UID, first: now}
			report.Suspected = append(report.Suspected, ns.Name)
			s.Metrics.orphan("suspected")
			s.Log.Info("orphaned preview namespace suspected; confirming on a later pass", "namespace", ns.Name)
			continue
		}
		if now.Sub(ns.CreationTimestamp.Time) < s.Grace || !now.After(prev.first) {
			continue
		}
		log := s.Log.WithValues("namespace", ns.Name, "tenant", ns.Labels[render.LabelTenant], "repo", ns.Labels[render.LabelRepo],
			"pr", ns.Labels[render.LabelPR], "env_id", ns.Labels[render.LabelEnv], "orphaned_since", prev.first.UTC())
		if s.DryRun {
			report.WouldDelete = append(report.WouldDelete, ns.Name)
			s.Metrics.orphan("dry_run")
			log.Info("dry run: would delete orphaned preview namespace")
			continue
		}
		log.Info("deleting orphaned preview namespace")
		if err := s.Destroy(ctx, ns.Name); err != nil {
			report.Failed = append(report.Failed, ns.Name)
			s.Metrics.orphan("failed")
			log.Error(err, "orphan deletion failed; will retry on a later pass")
			continue
		}
		delete(s.suspects, ns.Name)
		report.Deleted = append(report.Deleted, ns.Name)
		s.Metrics.orphan("deleted")
	}
	for name := range s.suspects {
		if !orphaned[name] {
			delete(s.suspects, name) // reappeared in desired state, or gone
		}
	}
	return report
}

// desired is every namespace the desired state or an existing object
// accounts for. Existing objects (including ones being deleted) belong to
// the controller, so the sweeper never races its destroy.
func (s *Sweeper) desired(ctx context.Context) (map[string]bool, error) {
	snap, err := s.Source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range snap.Environments {
		out[render.NamespaceFor(e.Spec.Repository, int(e.Spec.PullRequest), e.Spec.URLSuffix)] = true
	}
	var list v1alpha1.PreviewEnvironmentList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	for _, pe := range list.Items {
		out[render.NamespaceFor(pe.Spec.Repository, int(pe.Spec.PullRequest), pe.Spec.URLSuffix)] = true
	}
	return out, nil
}

func owned(ns *corev1.Namespace) bool {
	if !strings.HasPrefix(ns.Name, render.NamespacePrefix) {
		return false
	}
	for _, k := range requiredLabels {
		if ns.Labels[k] == "" {
			return false
		}
	}
	return true
}

// Metrics describe sweeper activity.
type Metrics struct {
	passes  *prometheus.CounterVec
	orphans *prometheus.CounterVec
}

// NewMetrics registers sweeper metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		passes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "heimdall_agent_sweeper_passes_total",
			Help: "Sweeper passes by result; only ok passes can delete."}, []string{"result"}),
		orphans: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "heimdall_agent_sweeper_orphans_total",
			Help: "Orphaned preview namespaces by action."}, []string{"action"}),
	}
	reg.MustRegister(m.passes, m.orphans)
	return m
}

func (m *Metrics) pass(result string) {
	if m != nil {
		m.passes.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) orphan(action string) {
	if m != nil {
		m.orphans.WithLabelValues(action).Inc()
	}
}
