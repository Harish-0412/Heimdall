package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const minimalConfig = "platform: {baseDomain: preview.example.com}\n"

func TestConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"source cluster":       c.Source.Type == SourceCluster,
		"sync 30s":             c.Source.SyncInterval.Duration == 30*time.Second,
		"4 operations":         c.Operations.MaxConcurrent == 4,
		"step 10m":             c.Operations.StepTimeout.Duration == 10*time.Minute,
		"sweeper on":           *c.Sweeper.Enabled && !c.Sweeper.DryRun,
		"grace 30m":            c.Sweeper.GracePeriod.Duration == 30*time.Minute,
		"admission required":   *c.Admission.RequirePolicy,
		"visibility private":   c.Policy.MaxVisibility == config.VisibilityPrivate,
		"large not allowed":    !c.Policy.AllowLargeSize,
		"https":                c.Platform.Platform().URLScheme == "",
		"configmap key unused": c.Source.Key == "",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("default %s not applied: %+v", name, c)
		}
	}
	p, err := c.Policy.Policy()
	if err != nil || p.MaxServices != config.DefaultLimits().MaxServices || p.AllowedSecrets != nil {
		t.Errorf("policy: %+v %v", p, err)
	}
}

func TestConfigPolicyConversion(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig + `policy:
  maxServices: 3
  maxContainerCPU: 1500m
  maxContainerMemory: 2Gi
  maxTotalCPU: "4"
  minMemoryRequestPercent: 60
  maxTTL: 72h
  maxVisibility: org
  allowedSecrets: []
  allowedRegistries: [ghcr.io/acme/]
  allowLargeSize: true
`))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Policy.Policy()
	if p.MaxServices != 3 || p.MaxContainerCPUMilli != 1500 || p.MaxContainerMemoryMi != 2048 || p.MaxTotalCPUMilli != 4000 ||
		p.MinMemoryRequestPercent != 60 || p.MaxTTL != 72*time.Hour || p.MaxVisibility != config.VisibilityOrg ||
		p.AllowedSecrets == nil || len(p.AllowedSecrets) != 0 || p.AllowedRegistries[0] != "ghcr.io/acme/" || !p.AllowLargeSize {
		t.Fatalf("policy: %+v", p)
	}
}

func TestConfigRejects(t *testing.T) {
	tests := map[string]string{
		"unknown field":        minimalConfig + "sweeper: {graceperiod: 1m}\n",
		"no base domain":       "source: {type: cluster}\n",
		"bad source type":      minimalConfig + "source: {type: api}\n",
		"configmap needs name": minimalConfig + "source: {type: configmap}\n",
		"file needs path":      minimalConfig + "source: {type: file}\n",
		"bad visibility":       minimalConfig + "policy: {maxVisibility: everyone}\n",
		"bad quantity":         minimalConfig + "policy: {maxTotalCPU: lots}\n",
		"ttl inverted":         minimalConfig + "policy: {minTTL: 48h, maxTTL: 1h}\n",
		"tiny interval":        minimalConfig + "sweeper: {interval: 10ms}\n",
		"metadata egress":      "platform: {baseDomain: preview.example.com, egressCIDRs: [169.254.169.254/32]}\n",
		"too many operations":  minimalConfig + "operations: {maxConcurrent: 1000}\n",
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(doc)); err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
		})
	}
}

func TestConfigReportsEveryProblem(t *testing.T) {
	_, err := ParseConfig([]byte("source: {type: configmap}\npolicy: {maxVisibility: x}\n"))
	for _, want := range []string{"source.configMap", "maxVisibility", "baseDomain"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
	}
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

func TestAccessBindsThePreviewRoleIdempotently(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	access := NewAccess(c, c, "heimdall-agent-preview-manager", "heimdall-system", "heimdall-agent")
	ctx := context.Background()
	for range 2 {
		if err := access(ctx, "heimdall-pr7-demo-abcd"); err != nil {
			t.Fatal(err)
		}
	}
	rb := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "heimdall-pr7-demo-abcd", Name: accessBinding}, rb); err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != "heimdall-agent-preview-manager" || rb.Subjects[0].Name != "heimdall-agent" || rb.Subjects[0].Namespace != "heimdall-system" {
		t.Fatalf("binding: %+v", rb)
	}
}

func TestAccessRefusesAForeignBinding(t *testing.T) {
	foreign := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: accessBinding, Namespace: "heimdall-pr7-demo-abcd"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "heimdall-agent", Namespace: "heimdall-system"}}}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(foreign).Build()
	err := NewAccess(c, c, "heimdall-agent-preview-manager", "heimdall-system", "heimdall-agent")(context.Background(), "heimdall-pr7-demo-abcd")
	if err == nil {
		t.Fatal("a binding to another role was accepted as ours")
	}
}

func TestAdmissionGuardDecision(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "probe",
		errors.New("ValidatingAdmissionPolicy 'heimdall-agent-namespaces' with binding 'heimdall-agent-namespaces' denied request: no"))
	tests := []struct {
		name     string
		create   error
		enforced bool
	}{
		{"denied by the policy", denied, true},
		{"admitted", nil, false},
		{"denied by something else", apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "probe", errors.New("RBAC")), false},
		{"API unavailable", errors.New("connection refused"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dryRun bool
			c := fake.NewClientBuilder().WithScheme(scheme()).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.CreateOption) error {
					o := &client.CreateOptions{}
					o.ApplyOptions(opts)
					dryRun = len(o.DryRun) == 1 && o.DryRun[0] == metav1.DryRunAll
					return tc.create
				}}).Build()
			g := &AdmissionGuard{Client: c, Policy: "heimdall-agent-namespaces", Log: logr.Discard()}
			enforced, _ := g.check(context.Background())
			if enforced != tc.enforced || !dryRun {
				t.Fatalf("enforced = %v (want %v), dry run = %v", enforced, tc.enforced, dryRun)
			}
		})
	}
}
