package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
)

type ControlPlane interface {
	Desired(context.Context) (gen.DesiredSnapshot, error)
}
type BundleLoader func(context.Context, string, string) (*bundle.Contents, error)

// PreparationFailure records an artifact failure before the generation has a
// cluster projection. The underlying error stays local; outbound observations
// carry only a stable public explanation.
type PreparationFailure struct {
	Generation int64
	At         metav1.Time
}

// API is the authoritative, outbound control-plane source. Failed reads do
// not change desired state. Every replica warms the policy used by admission;
// only the elected leader projects environments into the cluster.
type API struct {
	Control              ControlPlane
	Client               client.Client
	Namespace, ClusterID string
	Interval             time.Duration
	LoadBundle           BundleLoader
	mu                   sync.RWMutex
	tenant               string
	policy               config.Policy
	policyAt             time.Time
	revision             int64
	records              map[string]gen.Environment
	preparationFailures  map[string]PreparationFailure
}

func (a *API) Name() string             { return "api" }
func (a *API) Managed() bool            { return true }
func (a *API) NeedLeaderElection() bool { return false }
func (a *API) Start(ctx context.Context) error {
	interval := a.Interval
	if interval < time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := a.pull(ctx); err == nil {
			_ = a.pruneUnownedData(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// A crash can leave a fixture between materialization and CR creation. Remove
// unowned leftovers only after a fresh authoritative read, one hour of grace,
// and proof that no current environment or CR references the object.
func (a *API) pruneUnownedData(ctx context.Context) error {
	if a.Client == nil {
		return nil
	}
	records := a.Records()
	used := map[string]bool{}
	for _, e := range records {
		var spec v1alpha1.PreviewEnvironmentSpec
		if json.Unmarshal(e.Spec, &spec) == nil && spec.Data != nil {
			used[spec.Data.ConfigMap] = true
		}
	}
	var environments v1alpha1.PreviewEnvironmentList
	if err := a.Client.List(ctx, &environments, client.InNamespace(a.Namespace)); err != nil {
		return err
	}
	for _, pe := range environments.Items {
		if pe.Spec.Data != nil {
			used[pe.Spec.Data.ConfigMap] = true
		}
	}
	var maps corev1.ConfigMapList
	if err := a.Client.List(ctx, &maps, client.InNamespace(a.Namespace), client.MatchingLabels{LabelSource: "api"}); err != nil {
		return err
	}
	for i := range maps.Items {
		cm := &maps.Items[i]
		if used[cm.Name] || len(cm.OwnerReferences) > 0 || cm.CreationTimestamp.IsZero() || time.Since(cm.CreationTimestamp.Time) < time.Hour || cm.Immutable == nil || !*cm.Immutable || cm.Labels["heimdall.dev/env"] == "" || !strings.HasPrefix(cm.Name, "heimdall-data-"+cm.Labels["heimdall.dev/env"]+"-") {
			continue
		}
		uid := cm.UID
		if err := a.Client.Delete(ctx, cm, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
func (a *API) PolicyReady() error {
	a.mu.RLock()
	tenant := a.tenant
	a.mu.RUnlock()
	_, err := a.PolicyFor(context.Background(), tenant)
	return err
}

func (a *API) PolicyFor(_ context.Context, tenant string) (config.Policy, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ttl := 2 * a.Interval
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > 2*time.Minute {
		ttl = 2 * time.Minute
	}
	if tenant != a.tenant || a.policyAt.IsZero() || time.Since(a.policyAt) > ttl {
		return config.Policy{}, errors.New("authoritative tenant policy is unavailable")
	}
	return a.policy, nil
}

func (a *API) pull(ctx context.Context) (gen.DesiredSnapshot, error) {
	s, err := a.Control.Desired(ctx)
	if err != nil {
		return s, err
	}
	revision, err := strconv.ParseInt(s.Revision, 10, 64)
	if err != nil || revision < 0 || s.Environments == nil {
		return s, errors.New("invalid authoritative snapshot")
	}
	// Empty tenant snapshots still carry identity, so admission can warm up.
	tenant := s.TenantSlug
	if tenant == "" || s.TenantID == "" || s.Policy.AllowedSecrets == nil || s.Policy.AllowedRegistries == nil || s.Policy.MaxTotalCPUMilli <= 0 || s.Policy.MaxTotalMemoryMi <= 0 {
		return s, errors.New("snapshot has no explicit tenant policy")
	}
	records := map[string]gen.Environment{}
	for _, e := range s.Environments {
		if e.ClusterID != a.ClusterID || e.TenantID != s.TenantID || e.Id == "" || e.Name == "" || records[e.Id].Id != "" {
			return s, errors.New("snapshot identity or scope mismatch")
		}
		records[e.Id] = e
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if revision < a.revision {
		return s, errors.New("desired-state revision decreased")
	}
	a.tenant, a.policy, a.policyAt, a.revision, a.records = tenant, s.Policy, time.Now(), revision, records
	for id, failure := range a.preparationFailures {
		e, exists := records[id]
		if !exists || !preparationCurrent(e, failure.Generation) {
			delete(a.preparationFailures, id)
		}
	}
	return s, nil
}

// PreparationFailures returns only observations fenced to the current committed
// generation. Pending builds and destroyed environments cannot inherit errors
// from a previous artifact.
func (a *API) PreparationFailures() map[string]PreparationFailure {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]PreparationFailure, len(a.preparationFailures))
	for id, failure := range a.preparationFailures {
		e, exists := a.records[id]
		if exists && preparationCurrent(e, failure.Generation) {
			out[id] = failure
		}
	}
	return out
}

func (a *API) observePreparation(e *Environment, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, exists := a.records[e.Spec.EnvironmentID]
	if !exists || current.Generation != e.Spec.Generation {
		return
	}
	if err == nil || !preparationCurrent(current, e.Spec.Generation) {
		delete(a.preparationFailures, e.Spec.EnvironmentID)
		return
	}
	if a.preparationFailures == nil {
		a.preparationFailures = map[string]PreparationFailure{}
	}
	if failure, exists := a.preparationFailures[e.Spec.EnvironmentID]; !exists || failure.Generation != e.Spec.Generation {
		a.preparationFailures[e.Spec.EnvironmentID] = PreparationFailure{Generation: e.Spec.Generation, At: metav1.Now()}
	}
}

func preparationCurrent(e gen.Environment, generation int64) bool {
	if e.Generation != generation || e.DesiredState != gen.EnvironmentDesiredStateRunning {
		return false
	}
	var committed struct {
		EnvironmentID string `json:"environmentID"`
		Generation    int64  `json:"generation"`
	}
	return json.Unmarshal(e.Spec, &committed) == nil && committed.EnvironmentID == e.Id && committed.Generation == generation
}

func (a *API) Records() map[string]gen.Environment {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]gen.Environment, len(a.records))
	for k, v := range a.records {
		out[k] = v
	}
	return out
}

func (a *API) Snapshot(ctx context.Context) (*Snapshot, error) {
	s, err := a.pull(ctx)
	if err != nil {
		return nil, err
	}
	out := &Snapshot{Revision: s.Revision, Environments: []Environment{}}
	seen := map[string]bool{}
	for _, e := range s.Environments {
		if len(e.Spec) == 0 || string(e.Spec) == "{}" || string(e.Spec) == "null" {
			continue
		} // new PR waiting for its first CI build
		var spec v1alpha1.PreviewEnvironmentSpec
		d := json.NewDecoder(bytes.NewReader(e.Spec))
		d.DisallowUnknownFields()
		if d.Decode(&spec) != nil || spec.EnvironmentID != e.Id || spec.Tenant != s.TenantSlug || spec.Generation < 1 || spec.Generation > e.Generation || seen[e.Name] {
			return nil, errors.New("invalid environment projection")
		}
		seen[e.Name] = true
		// Preserve the committed generation during a pending replacement build.
		// Overwriting it with e.Generation would relabel the old image as new.
		out.Environments = append(out.Environments, Environment{Name: e.Name, Spec: spec})
	}
	return out, nil
}

// Prepare isolates an unavailable artifact to its own environment. Other PRs
// and cleanup can still converge from the same authoritative snapshot.
func (a *API) Prepare(ctx context.Context, e *Environment) (err error) {
	defer func() { a.observePreparation(e, err) }()
	if e.Spec.DesiredState == v1alpha1.DesiredDestroyed || e.Spec.Config.Bundle == "" {
		return nil
	}
	p, err := a.PolicyFor(ctx, e.Spec.Tenant)
	if err != nil {
		return err
	}
	if err = a.materialize(ctx, &e.Spec, p); err != nil {
		return fmt.Errorf("bundle unavailable: %w", err)
	}
	return nil
}

// AfterApply binds fixture lifetime to the exact PreviewEnvironment UID. Old
// imports are retained during a rollout, and pruned after successful convergence
// or when the environment is explicitly destroyed.
func (a *API) AfterApply(ctx context.Context, env Environment) error {
	if a.Client == nil {
		return nil
	}
	var pe v1alpha1.PreviewEnvironment
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: env.Name}, &pe); err != nil {
		return err
	}
	if pe.Spec.EnvironmentID != env.Spec.EnvironmentID || pe.Spec.Generation != env.Spec.Generation || pe.Labels[LabelSource] != "api" || pe.UID == "" {
		return errors.New("fixture owner identity is unavailable")
	}
	var maps corev1.ConfigMapList
	if err := a.Client.List(ctx, &maps, client.InNamespace(a.Namespace), client.MatchingLabels{LabelSource: "api", "heimdall.dev/env": env.Spec.EnvironmentID}); err != nil {
		return err
	}
	destroyed := pe.Spec.DesiredState == v1alpha1.DesiredDestroyed
	ready := pe.Status.Phase == v1alpha1.PhaseReady && pe.Status.DeployedGeneration == pe.Spec.Generation
	for i := range maps.Items {
		cm := &maps.Items[i]
		if !strings.HasPrefix(cm.Name, "heimdall-data-"+env.Spec.EnvironmentID+"-") || cm.Immutable == nil || !*cm.Immutable {
			continue
		}
		current := pe.Spec.Data != nil && pe.Spec.Data.ConfigMap == cm.Name
		owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "PreviewEnvironment", Name: pe.Name, UID: pe.UID}
		if len(cm.OwnerReferences) > 0 && !equality.Semantic.DeepEqual(cm.OwnerReferences, []metav1.OwnerReference{owner}) {
			return errors.New("fixture already belongs to another owner")
		}
		if destroyed || ready && !current {
			uid := cm.UID
			if err := a.Client.Delete(ctx, cm, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
				return err
			}
			continue
		}
		if len(cm.OwnerReferences) == 0 {
			cm.OwnerReferences = []metav1.OwnerReference{owner}
			if err := a.Client.Update(ctx, cm); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *API) materialize(ctx context.Context, s *v1alpha1.PreviewEnvironmentSpec, p config.Policy) error {
	allowed := false
	for _, prefix := range p.AllowedRegistries {
		if strings.HasPrefix(s.Config.Bundle, prefix) {
			allowed = true
			break
		}
	}
	if !allowed || a.LoadBundle == nil {
		return errors.New("bundle registry is not permitted or unavailable")
	}
	b, err := a.LoadBundle(ctx, s.Config.Bundle, s.Config.SHA256)
	if err != nil {
		return err
	}
	if !bytes.Equal(b.Config, []byte(s.Config.Inline)) {
		return errors.New("bundle and approved configuration differ")
	}
	if len(b.Seed) == 0 {
		if s.Data != nil {
			return errors.New("approved data is missing from bundle")
		}
		return nil
	}
	if s.Data == nil || !s.Data.Approval.Sanitised || s.Data.Approval.ApprovedBy == "" || s.Data.Approval.Reason == "" || len(s.Data.Approval.SHA256) != 64 || bundle.ConfigDigest(b.Seed) != s.Data.Approval.SHA256 {
		return errors.New("bundle data lacks an exact sanitised-data attestation")
	}
	if a.Client == nil {
		return errors.New("cluster data storage is unavailable")
	}
	name := s.Data.ConfigMap
	// Only write data to the reserved object names, in this agent's namespace.
	if name != "heimdall-data-"+s.EnvironmentID+"-"+s.Data.Approval.SHA256[:12] || s.Data.Key != "seed.sql" {
		return errors.New("unexpected data object name")
	}
	var existing corev1.ConfigMap
	err = a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: name}, &existing)
	if err == nil {
		if existing.Labels[LabelSource] != "api" || existing.Labels["heimdall.dev/env"] != s.EnvironmentID || existing.Immutable == nil || !*existing.Immutable || existing.Data["seed.sql"] != string(b.Seed) {
			return errors.New("data object ownership or contents differ")
		}
		return nil
	}
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: name, Labels: map[string]string{LabelSource: "api", "heimdall.dev/env": s.EnvironmentID}}, Immutable: new(true), Data: map[string]string{"seed.sql": string(b.Seed)}}
	return a.Client.Create(ctx, cm)
}
