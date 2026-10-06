package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/controlclient"
	"github.com/heimdall-dev/heimdall/internal/controller"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/source"
	"github.com/heimdall-dev/heimdall/internal/sweeper"
	"github.com/heimdall-dev/heimdall/internal/version"
	"github.com/heimdall-dev/heimdall/internal/webhook"
)

// Options are the process-level settings (flags), as opposed to Config,
// which is the operator's configuration file.
type Options struct {
	Config *Config
	// Namespace is the agent's own namespace: its PreviewEnvironments,
	// leader-election lease, webhook certificate and desired-state ConfigMap.
	Namespace string
	// ServiceAccount is the agent's service account name.
	ServiceAccount string
	// PreviewManagerRole is bound in each preview namespace (tier 2).
	PreviewManagerRole string
	// NamespacePolicy is the ValidatingAdmissionPolicy the guard expects.
	NamespacePolicy string
	// Webhook settings.
	WebhookPort          int
	WebhookService       string
	WebhookConfiguration string
	WebhookSecret        string
	// Endpoints.
	MetricsAddr   string
	SecureMetrics bool
	ProbeAddr     string
	// LeaderElection allows several replicas; one reconciles at a time.
	LeaderElection bool
	Log            logr.Logger
}

const leaderElectionID = "heimdall-agent.heimdall.dev"

// Run starts the agent and blocks until ctx is cancelled or it fails.
func Run(ctx context.Context, rc *rest.Config, o Options) error {
	cfg := o.Config
	policy, err := cfg.Policy.Policy()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), v1alpha1.AddToScheme(scheme)); err != nil {
		return err
	}

	// HTTP/2 is disabled on every server: rapid-reset and stream-cancellation
	// attacks (CVE-2023-44487, CVE-2023-39325) target it, and webhooks and
	// metrics gain nothing from it.
	http1 := func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }
	certs := &webhook.Certificates{
		Namespace: o.Namespace, SecretName: o.WebhookSecret, WebhookConfig: o.WebhookConfiguration,
		DNSNames: []string{o.WebhookService + "." + o.Namespace + ".svc", o.WebhookService + "." + o.Namespace + ".svc.cluster.local"},
		Log:      o.Log.WithName("webhook-certificates"),
	}
	metricsOptions := metricsserver.Options{BindAddress: o.MetricsAddr, SecureServing: o.SecureMetrics, TLSOpts: []func(*tls.Config){http1}}
	if o.SecureMetrics {
		// Scrapers authenticate with a token and need get on /metrics
		// (the chart's metrics-reader ClusterRole).
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	mgr, err := ctrl.NewManager(rc, ctrl.Options{
		Scheme:  scheme,
		Logger:  o.Log,
		Metrics: metricsOptions,
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{Port: o.WebhookPort, TLSOpts: []func(*tls.Config){http1,
			func(c *tls.Config) { c.GetCertificate, c.MinVersion = certs.GetCertificate, tls.VersionTLS12 }}}),
		HealthProbeBindAddress: o.ProbeAddr,
		// The cache only ever holds the agent's own namespace: everything else
		// is read directly, scoped by RBAC.
		Cache:                         cache.Options{DefaultNamespaces: map[string]cache.Config{o.Namespace: {}}},
		LeaderElection:                o.LeaderElection,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       o.Namespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	certs.Client, certs.Reader = mgr.GetClient(), mgr.GetAPIReader()

	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	cluster := &engine.Kubernetes{Client: dyn}
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	stepTimeout := cfg.Operations.StepTimeout.Duration
	access := NewAccess(mgr.GetClient(), mgr.GetAPIReader(), o.PreviewManagerRole, o.Namespace, o.ServiceAccount)
	specs := controller.SpecBuilder{Policy: policy, Platform: cfg.Platform.Platform(), Data: configMapData(mgr.GetAPIReader(), o.Namespace)}
	var src source.Source
	var control *controlclient.Client
	if cfg.Source.Type == SourceAPI {
		p := cfg.Source.ControlPlane
		direct, err := client.New(rc, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		control, err = controlclient.New(p.URL, p.ClusterID, version.Version, &controlclient.SecretStore{Client: direct, Namespace: o.Namespace, Name: p.AuthSecret}, p.AllowLocalHTTP)
		if err != nil {
			return err
		}
		api := &source.API{Control: control, Client: direct, Namespace: o.Namespace, ClusterID: p.ClusterID, Interval: cfg.Source.SyncInterval.Duration,
			LoadBundle: func(ctx context.Context, ref, sha string) (*bundle.Contents, error) {
				return bundle.Fetch(ctx, ref, sha, bundle.RegistryOptions{PlainHTTP: p.RegistryPlainHTTP, DockerConfig: p.DockerConfig})
			}}
		specs.PolicyFor, src = api.PolicyFor, api
		if err := mgr.Add(api); err != nil {
			return err
		}
		if err := mgr.AddReadyzCheck("control-plane-policy", func(*http.Request) error { return api.PolicyReady() }); err != nil {
			return err
		}
		if err := mgr.Add(&Reporter{Control: control, Source: api, Reader: mgr.GetAPIReader(), Namespace: o.Namespace, Specs: specs, Engine: engine.New(cluster, stepTimeout, nil), Kubernetes: clientset, Log: o.Log.WithName("control-plane")}); err != nil {
			return err
		}
	} else {
		src, err = newSource(cfg.Source, mgr.GetAPIReader(), o.Namespace)
		if err != nil {
			return err
		}
	}

	var guard controller.Guard = controller.Allowed{}
	if *cfg.Admission.RequirePolicy {
		gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "heimdall_agent_admission_policy_enforced",
			Help: "1 once the agent's admission policy is observably enforced; the agent changes nothing until then."})
		metrics.Registry.MustRegister(gauge)
		g := &AdmissionGuard{Client: mgr.GetClient(), Policy: o.NamespacePolicy, Interval: 5 * time.Minute, Log: o.Log.WithName("admission-guard"), Gauge: gauge}
		if err := mgr.Add(g); err != nil {
			return err
		}
		if err := mgr.AddReadyzCheck("admission-policy", g.Ready); err != nil {
			return err
		}
		guard = g
	}

	notifier := controller.NewNotifier()
	reconciler := &controller.Reconciler{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorder("heimdall-agent"),
		Specs: specs, Runner: controller.NewRunner(controller.EngineOperators(cluster, stepTimeout), cfg.Operations.MaxConcurrent, notifier.Notify),
		Access: access, Guard: guard, Resync: cfg.Operations.ResyncInterval.Duration, MaxBackoff: cfg.Operations.MaxBackoff.Duration,
		Metrics: controller.NewMetrics(metrics.Registry), Diagnoser: clusterDiagnoser{client: clientset, dynamic: dyn},
	}
	if err := reconciler.SetupWithManager(mgr, notifier, 4); err != nil {
		return err
	}

	mgr.GetWebhookServer().Register(webhook.Path,
		admission.WithValidator[*v1alpha1.PreviewEnvironment](scheme, &webhook.Validator{Specs: specs}))
	if err := mgr.Add(certs); err != nil {
		return err
	}
	if err := errors.Join(mgr.AddReadyzCheck("webhook-certificate", certs.Ready), mgr.AddHealthzCheck("ping", healthz.Ping)); err != nil {
		return err
	}

	if src.Managed() {
		if err := mgr.Add(&source.Syncer{Source: src, Client: mgr.GetClient(), Namespace: o.Namespace,
			Interval: cfg.Source.SyncInterval.Duration, Metrics: source.NewMetrics(metrics.Registry), Log: o.Log.WithName("source")}); err != nil {
			return err
		}
	}
	if *cfg.Sweeper.Enabled {
		sw := &sweeper.Sweeper{
			Source: src, Reader: mgr.GetAPIReader(), Namespace: o.Namespace,
			Grace: cfg.Sweeper.GracePeriod.Duration, Interval: cfg.Sweeper.Interval.Duration, DryRun: cfg.Sweeper.DryRun,
			Metrics: sweeper.NewMetrics(metrics.Registry), Log: o.Log.WithName("sweeper"),
			Destroy: func(ctx context.Context, namespace string) error {
				if !guard.Allowed() {
					return errors.New("admission policy not enforced")
				}
				if err := access(ctx, namespace); err != nil {
					return err
				}
				_, err := engine.New(cluster, stepTimeout, nil).DestroyOrphan(ctx, namespace)
				return err
			},
		}
		if err := mgr.Add(sw); err != nil {
			return err
		}
	}
	metrics.Registry.MustRegister(&environmentCollector{reader: mgr.GetClient(), namespace: o.Namespace})

	o.Log.Info("starting agent", "namespace", o.Namespace, "source", src.Name(), "sweeper", *cfg.Sweeper.Enabled,
		"sweeper_dry_run", cfg.Sweeper.DryRun, "require_admission_policy", *cfg.Admission.RequirePolicy)
	return mgr.Start(ctx)
}

func newSource(c SourceConfig, reader client.Reader, namespace string) (source.Source, error) {
	switch c.Type {
	case SourceCluster:
		return &source.Cluster{Reader: reader, Namespace: namespace}, nil
	case SourceConfigMap:
		return &source.ConfigMap{Reader: reader, Namespace: namespace, Object: c.ConfigMap, Key: c.Key}, nil
	case SourceFile:
		return &source.File{Path: c.Path}, nil
	}
	return nil, fmt.Errorf("unknown source type %q", c.Type)
}

// configMapData resolves spec.data from a ConfigMap in the agent's namespace.
func configMapData(reader client.Reader, namespace string) controller.DataLoader {
	return func(ctx context.Context, name, key string) ([]byte, error) {
		cm := &corev1.ConfigMap{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm); err != nil {
			return nil, err
		}
		if v, ok := cm.Data[key]; ok {
			return []byte(v), nil
		}
		if v, ok := cm.BinaryData[key]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("ConfigMap %s has no key %q", name, key)
	}
}

// environmentCollector reports environments by phase from the cache.
type environmentCollector struct {
	reader    client.Reader
	namespace string
}

var environmentsDesc = prometheus.NewDesc("heimdall_agent_environments", "PreviewEnvironments by phase.", []string{"phase"}, nil)

func (c *environmentCollector) Describe(ch chan<- *prometheus.Desc) { ch <- environmentsDesc }

func (c *environmentCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var list v1alpha1.PreviewEnvironmentList
	if err := c.reader.List(ctx, &list, client.InNamespace(c.namespace)); err != nil {
		return
	}
	counts := map[string]float64{}
	for _, pe := range list.Items {
		phase := string(pe.Status.Phase)
		if phase == "" {
			phase = string(v1alpha1.PhasePending)
		}
		counts[phase]++
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(environmentsDesc, prometheus.GaugeValue, n, phase)
	}
}
