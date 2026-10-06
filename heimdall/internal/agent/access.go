package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/controller"
)

// accessBinding is the RoleBinding that grants the agent its tier-2 role
// inside a preview namespace (charts/heimdall-agent README).
const accessBinding = "heimdall-agent"

// NewAccess returns the controller's Access: it binds role (the chart's
// preview-manager ClusterRole) to the agent's service account inside a
// preview namespace. The agent holds "bind" on that one role only, and its
// admission policy confines where the binding may be created.
func NewAccess(c client.Client, reader client.Reader, role, saNamespace, saName string) controller.Access {
	return func(ctx context.Context, namespace string) error {
		want := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: accessBinding, Namespace: namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "heimdall-agent"}},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: saNamespace}},
		}
		err := c.Create(ctx, want)
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		have := &rbacv1.RoleBinding{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: accessBinding}, have); err != nil {
			return err
		}
		// roleRef is immutable: a binding to anything else is not ours to fix.
		if have.RoleRef != want.RoleRef || len(have.Subjects) != 1 || have.Subjects[0] != want.Subjects[0] {
			return fmt.Errorf("RoleBinding %s/%s exists with a different role or subject", namespace, accessBinding)
		}
		return nil
	}
}

// AdmissionGuard verifies that the agent's ValidatingAdmissionPolicy is
// enforced before the agent changes anything, and keeps checking. Policies
// take effect asynchronously after they are created; the P1 end-to-end test
// observed the gap right after installation. The check is a server-side dry
// run that creates a namespace without the preview label, which the policy
// must deny: nothing is persisted either way.
type AdmissionGuard struct {
	Client   client.Client
	Policy   string // the ValidatingAdmissionPolicy expected to deny
	Interval time.Duration
	Log      logr.Logger
	Gauge    prometheus.Gauge

	ok atomic.Bool
}

// Start implements manager.Runnable. Every replica checks (readiness).
func (g *AdmissionGuard) Start(ctx context.Context) error {
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	fast := time.NewTicker(2 * time.Second) // until first enforced
	defer fast.Stop()
	for {
		enforced, err := g.check(ctx)
		if enforced != g.ok.Load() || err != nil {
			g.Log.Info("admission policy check", "policy", g.Policy, "enforced", enforced, "detail", errString(err))
		}
		g.ok.Store(enforced)
		if g.Gauge != nil {
			g.Gauge.Set(map[bool]float64{true: 1, false: 0}[enforced])
		}
		tick := ticker.C
		if !enforced {
			tick = fast.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (g *AdmissionGuard) NeedLeaderElection() bool { return false }

// Allowed implements controller.Guard.
func (g *AdmissionGuard) Allowed() bool { return g.ok.Load() }

// Ready is a readiness check.
func (g *AdmissionGuard) Ready(*http.Request) error {
	if !g.ok.Load() {
		return errors.New("admission policy " + g.Policy + " is not enforced yet")
	}
	return nil
}

func (g *AdmissionGuard) check(ctx context.Context) (bool, error) {
	probe := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-admission-probe-" + strings.ToLower(rand.Text()[:8])}}
	err := g.Client.Create(ctx, probe, client.DryRunAll)
	switch {
	case err == nil:
		return false, errors.New("a namespace without the preview label was admitted")
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+g.Policy+"'"):
		return true, nil
	default:
		return false, err
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
