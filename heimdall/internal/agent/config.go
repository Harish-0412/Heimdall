// Package agent wires the in-cluster agent: configuration, the controller
// manager, the desired-state source and syncer, the sweeper, the admission
// webhook, the admission-policy guard and metrics (docs/agent.md).
package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/render"
	"github.com/heimdall-dev/heimdall/internal/strictyaml"
)

// Source types.
const (
	SourceCluster   = "cluster"   // PreviewEnvironment objects are the desired state (kubectl apply)
	SourceConfigMap = "configmap" // a DesiredState document in a ConfigMap in the agent's namespace
	SourceFile      = "file"      // a DesiredState document on disk (development)
)

// Config is the agent's configuration file (charts/heimdall-agent renders it
// from values). It is platform-operator input: pull requests cannot reach it.
type Config struct {
	Source     SourceConfig     `json:"source"`
	Platform   PlatformConfig   `json:"platform"`
	Policy     PolicyConfig     `json:"policy"`
	Operations OperationsConfig `json:"operations"`
	Sweeper    SweeperConfig    `json:"sweeper"`
	Admission  AdmissionConfig  `json:"admission"`
}

// SourceConfig selects where desired state comes from (ADR 0010).
type SourceConfig struct {
	Type string `json:"type"`
	// ConfigMap and Key locate the document for type configmap.
	ConfigMap string `json:"configMap,omitempty"`
	Key       string `json:"key,omitempty"`
	// Path locates the document for type file.
	Path string `json:"path,omitempty"`
	// SyncInterval is how often the source is read. Default 30s.
	SyncInterval metav1.Duration `json:"syncInterval,omitempty"`
}

// PlatformConfig mirrors render.Platform (docs/rendering.md).
type PlatformConfig struct {
	BaseDomain             string                           `json:"baseDomain"`
	URLScheme              string                           `json:"urlScheme,omitempty"`
	URLPort                int                              `json:"urlPort,omitempty"`
	Gateway                GatewayConfig                    `json:"gateway,omitempty"`
	IngressPeers           []networkingv1.NetworkPolicyPeer `json:"ingressPeers,omitempty"`
	DNSPeers               []networkingv1.NetworkPolicyPeer `json:"dnsPeers,omitempty"`
	EgressCIDRs            []string                         `json:"egressCIDRs,omitempty"`
	StorageClassName       string                           `json:"storageClassName,omitempty"`
	NodeSelector           map[string]string                `json:"nodeSelector,omitempty"`
	Tolerations            []corev1.Toleration              `json:"tolerations,omitempty"`
	RuntimeClassName       string                           `json:"runtimeClassName,omitempty"`
	ImageMirror            string                           `json:"imageMirror,omitempty"`
	WritableRootFilesystem bool                             `json:"writableRootFilesystem,omitempty"`
}

// GatewayConfig names the shared preview Gateway.
type GatewayConfig struct {
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
	SectionName string `json:"sectionName,omitempty"`
}

// PolicyConfig is the tenant policy (ADR 0006) until the control plane
// delivers it (P5). Omitted limits take the platform defaults.
type PolicyConfig struct {
	MaxServices             int             `json:"maxServices,omitempty"`
	MaxWorkers              int             `json:"maxWorkers,omitempty"`
	MaxContainerCPU         string          `json:"maxContainerCPU,omitempty"`
	MaxContainerMemory      string          `json:"maxContainerMemory,omitempty"`
	MaxTotalCPU             string          `json:"maxTotalCPU,omitempty"`
	MaxTotalMemory          string          `json:"maxTotalMemory,omitempty"`
	MaxPostgresStorage      string          `json:"maxPostgresStorage,omitempty"`
	MinMemoryRequestPercent *int            `json:"minMemoryRequestPercent,omitempty"`
	MinTTL                  metav1.Duration `json:"minTTL,omitempty"`
	MaxTTL                  metav1.Duration `json:"maxTTL,omitempty"`
	// MaxVisibility defaults to private.
	MaxVisibility string `json:"maxVisibility,omitempty"`
	// AllowedSecrets: omitted means unrestricted; [] means none.
	AllowedSecrets []string `json:"allowedSecrets,omitempty"`
	// AllowedRegistries: omitted means unrestricted; [] means build only.
	AllowedRegistries []string `json:"allowedRegistries,omitempty"`
	AllowLargeSize    bool     `json:"allowLargeSize,omitempty"`
}

// OperationsConfig bounds engine work.
type OperationsConfig struct {
	// MaxConcurrent engine operations across all environments. Default 4.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// StepTimeout bounds each engine step. Default 10m.
	StepTimeout metav1.Duration `json:"stepTimeout,omitempty"`
	// ResyncInterval re-checks Ready environments for drift. Default 5m.
	ResyncInterval metav1.Duration `json:"resyncInterval,omitempty"`
	// MaxBackoff caps retry delays after retryable failures. Default 10m.
	MaxBackoff metav1.Duration `json:"maxBackoff,omitempty"`
}

// SweeperConfig controls orphan removal (ADR 0005's sweeper safety rules).
type SweeperConfig struct {
	// Enabled defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
	// DryRun only reports orphans.
	DryRun bool `json:"dryRun,omitempty"`
	// Interval between passes. Default 5m.
	Interval metav1.Duration `json:"interval,omitempty"`
	// GracePeriod an orphan must exist (and be seen orphaned) before
	// deletion. Default 30m.
	GracePeriod metav1.Duration `json:"gracePeriod,omitempty"`
}

// AdmissionConfig controls the admission-policy self-check.
type AdmissionConfig struct {
	// RequirePolicy makes the agent refuse to act until its
	// ValidatingAdmissionPolicy is observably enforced. Default true.
	RequirePolicy *bool `json:"requirePolicy,omitempty"`
}

// LoadConfig reads and validates the agent configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig decodes strictly (unknown or duplicate fields are errors),
// applies defaults and validates.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	if err := strictyaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Source.Type == "" {
		c.Source.Type = SourceCluster
	}
	if c.Source.Type == SourceConfigMap && c.Source.Key == "" {
		c.Source.Key = "desired-state.yaml"
	}
	durationDefault(&c.Source.SyncInterval, 30*time.Second)
	if c.Policy.MaxVisibility == "" {
		c.Policy.MaxVisibility = config.VisibilityPrivate
	}
	if c.Operations.MaxConcurrent == 0 {
		c.Operations.MaxConcurrent = 4
	}
	durationDefault(&c.Operations.StepTimeout, 10*time.Minute)
	durationDefault(&c.Operations.ResyncInterval, 5*time.Minute)
	durationDefault(&c.Operations.MaxBackoff, 10*time.Minute)
	if c.Sweeper.Enabled == nil {
		c.Sweeper.Enabled = new(true)
	}
	durationDefault(&c.Sweeper.Interval, 5*time.Minute)
	durationDefault(&c.Sweeper.GracePeriod, 30*time.Minute)
	if c.Admission.RequirePolicy == nil {
		c.Admission.RequirePolicy = new(true)
	}
}

// configPath turns a render field path ("Context.Platform.URLScheme") into
// the configuration key an operator writes ("platform.urlScheme").
func configPath(field string) string {
	parts := strings.Split(strings.TrimPrefix(field, "Context."), ".")
	for i, p := range parts {
		// Lower-case the leading capitals, keeping the last one of an acronym
		// followed by a word: URLScheme -> urlScheme, EgressCIDRs -> egressCIDRs.
		r := []rune(p)
		n := 0
		for n < len(r) && r[n] >= 'A' && r[n] <= 'Z' {
			n++
		}
		if n > 1 && n < len(r) && r[n] >= 'a' && r[n] <= 'z' {
			n--
		}
		for j := range n {
			r[j] += 'a' - 'A'
		}
		parts[i] = string(r)
	}
	return strings.Join(parts, ".")
}

func durationDefault(d *metav1.Duration, def time.Duration) {
	if d.Duration == 0 {
		d.Duration = def
	}
}

// validate reports every problem at once.
func (c *Config) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch c.Source.Type {
	case SourceCluster:
	case SourceConfigMap:
		if c.Source.ConfigMap == "" {
			bad("source.configMap is required for type configmap")
		}
	case SourceFile:
		if c.Source.Path == "" {
			bad("source.path is required for type file")
		}
	default:
		bad("source.type %q must be cluster, configmap or file", c.Source.Type)
	}
	if _, err := c.Policy.Policy(); err != nil {
		errs = append(errs, err)
	}
	// Platform fields are validated by render, against the real rules, and
	// reported with the paths an operator writes ("platform.baseDomain").
	var platformErrs render.Errors
	if err := render.ValidatePlatform(c.Platform.Platform()); errors.As(err, &platformErrs) {
		for _, e := range platformErrs {
			bad("%s: %s", configPath(e.Field), e.Message)
		}
	} else if err != nil {
		errs = append(errs, err)
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"source.syncInterval", c.Source.SyncInterval.Duration}, {"operations.stepTimeout", c.Operations.StepTimeout.Duration},
		{"operations.resyncInterval", c.Operations.ResyncInterval.Duration}, {"operations.maxBackoff", c.Operations.MaxBackoff.Duration},
		{"sweeper.interval", c.Sweeper.Interval.Duration}, {"sweeper.gracePeriod", c.Sweeper.GracePeriod.Duration},
	} {
		if d.v < time.Second {
			bad("%s must be at least 1s", d.name)
		}
	}
	if c.Operations.MaxConcurrent < 1 || c.Operations.MaxConcurrent > 64 {
		bad("operations.maxConcurrent must be between 1 and 64")
	}
	if len(errs) > 0 {
		return fmt.Errorf("agent config: %w", errors.Join(errs...))
	}
	return nil
}

// Platform converts to render.Platform.
func (p PlatformConfig) Platform() render.Platform {
	return render.Platform{
		BaseDomain: p.BaseDomain, URLScheme: p.URLScheme, URLPort: p.URLPort,
		Gateway:      render.GatewayRef{Namespace: p.Gateway.Namespace, Name: p.Gateway.Name, SectionName: p.Gateway.SectionName},
		IngressPeers: p.IngressPeers, DNSPeers: p.DNSPeers, EgressCIDRs: p.EgressCIDRs,
		StorageClassName: p.StorageClassName, NodeSelector: p.NodeSelector, Tolerations: p.Tolerations,
		RuntimeClassName: p.RuntimeClassName, ImageMirror: p.ImageMirror, WritableRootFilesystem: p.WritableRootFilesystem,
	}
}

// Policy converts to config.Policy, starting from the platform defaults.
func (p PolicyConfig) Policy() (config.Policy, error) {
	out := config.Policy{Limits: config.DefaultLimits(), MaxVisibility: p.MaxVisibility, AllowLargeSize: p.AllowLargeSize}
	out.AllowedSecrets, out.AllowedRegistries = p.AllowedSecrets, p.AllowedRegistries
	var errs []error
	positive := func(name string, v int, dst *int) {
		if v < 0 {
			errs = append(errs, fmt.Errorf("policy.%s must not be negative", name))
		} else if v > 0 {
			*dst = v
		}
	}
	positive("maxServices", p.MaxServices, &out.MaxServices)
	positive("maxWorkers", p.MaxWorkers, &out.MaxWorkers)
	quantity := func(name, s string, milli bool, dst *int) {
		if s == "" {
			return
		}
		q, err := resource.ParseQuantity(s)
		switch {
		case err != nil || q.Sign() <= 0:
			errs = append(errs, fmt.Errorf("policy.%s %q is not a positive quantity", name, s))
		case milli:
			*dst = int(q.MilliValue())
		default:
			*dst = int(q.Value() >> 20)
		}
	}
	quantity("maxContainerCPU", p.MaxContainerCPU, true, &out.MaxContainerCPUMilli)
	quantity("maxContainerMemory", p.MaxContainerMemory, false, &out.MaxContainerMemoryMi)
	quantity("maxTotalCPU", p.MaxTotalCPU, true, &out.MaxTotalCPUMilli)
	quantity("maxTotalMemory", p.MaxTotalMemory, false, &out.MaxTotalMemoryMi)
	quantity("maxPostgresStorage", p.MaxPostgresStorage, false, &out.MaxPostgresStorageMi)
	if v := p.MinMemoryRequestPercent; v != nil {
		if *v < 0 || *v > 100 {
			errs = append(errs, fmt.Errorf("policy.minMemoryRequestPercent must be 0-100"))
		}
		out.MinMemoryRequestPercent = *v
	}
	if p.MinTTL.Duration > 0 {
		out.MinTTL = p.MinTTL.Duration
	}
	if p.MaxTTL.Duration > 0 {
		out.MaxTTL = p.MaxTTL.Duration
	}
	if out.MinTTL > out.MaxTTL {
		errs = append(errs, fmt.Errorf("policy.minTTL must not exceed policy.maxTTL"))
	}
	switch p.MaxVisibility {
	case config.VisibilityPrivate, config.VisibilityOrg, config.VisibilityPublic:
	default:
		errs = append(errs, fmt.Errorf("policy.maxVisibility %q must be private, org or public", p.MaxVisibility))
	}
	for _, s := range p.AllowedSecrets {
		if strings.TrimSpace(s) == "" {
			errs = append(errs, fmt.Errorf("policy.allowedSecrets must not contain empty names"))
		}
	}
	return out, errors.Join(errs...)
}
