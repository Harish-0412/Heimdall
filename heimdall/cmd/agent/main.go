// Command agent runs in each target cluster: the PreviewEnvironment
// controller, its desired-state source, the orphan sweeper and the admission
// webhook (docs/agent.md). It connects out to the Kubernetes API (and, from
// P5, to the Heimdall control plane); nothing connects in except the API
// server calling the webhook and a metrics scraper.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/heimdall-dev/heimdall/internal/agent"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"github.com/heimdall-dev/heimdall/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	stopTrace := tracecontext.Initialize()
	defer func() { _ = stopTrace(context.Background()) }()
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/heimdall/agent.yaml", "agent configuration file")
	metricsAddr := fs.String("metrics-bind-address", ":8443", `metrics endpoint ("0" disables)`)
	secureMetrics := fs.Bool("metrics-secure", true, "serve metrics over HTTPS with Kubernetes authentication and authorization")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "health and readiness probes")
	webhookPort := fs.Int("webhook-port", 9443, "admission webhook port")
	leaderElect := fs.Bool("leader-elect", true, "elect a leader so that several replicas can run")
	serviceAccount := fs.String("service-account", "heimdall-agent", "the agent's service account")
	previewRole := fs.String("preview-manager-role", "heimdall-agent-preview-manager", "ClusterRole bound in each preview namespace")
	namespacePolicy := fs.String("namespace-policy", "heimdall-agent-namespaces", "ValidatingAdmissionPolicy that confines namespace writes")
	webhookService := fs.String("webhook-service", "heimdall-agent-webhook", "Service in front of the admission webhook")
	webhookConfig := fs.String("webhook-configuration", "heimdall-agent", "ValidatingWebhookConfiguration to inject the CA into")
	webhookSecret := fs.String("webhook-secret", "heimdall-agent-webhook-tls", "Secret holding the webhook CA and certificate")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	checkConfig := fs.Bool("check-config", false, "validate the configuration file and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Printf("heimdall-agent %s (commit %s)\n", version.Version, version.Commit)
		return 0
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "agent: --log-level: %v\n", err)
		return 2
	}
	// JSON logs (cross-cutting rule); client-go's klog goes to the same sink.
	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctrl.SetLogger(log)
	klog.SetLogger(log)

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Error(err, "invalid configuration")
		return 2
	}
	if *checkConfig {
		fmt.Printf("%s: valid\n", *configPath)
		return 0
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		log.Error(nil, "POD_NAMESPACE must be set (the chart sets it with the downward API)")
		return 2
	}
	rc, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "no Kubernetes configuration")
		return 2
	}
	rc.UserAgent = "heimdall-agent/" + version.Version

	err = agent.Run(ctrl.SetupSignalHandler(), rc, agent.Options{
		Config: cfg, Namespace: namespace, ServiceAccount: *serviceAccount, PreviewManagerRole: *previewRole,
		NamespacePolicy: *namespacePolicy, WebhookPort: *webhookPort, WebhookService: *webhookService,
		WebhookConfiguration: *webhookConfig, WebhookSecret: *webhookSecret, MetricsAddr: *metricsAddr,
		SecureMetrics: *secureMetrics, ProbeAddr: *probeAddr, LeaderElection: *leaderElect,
		Log: log.WithValues("version", version.Version),
	})
	if err != nil {
		log.Error(err, "agent stopped")
		return 1
	}
	return 0
}
