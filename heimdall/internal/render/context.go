package render

import (
	"crypto/rand"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// Context is everything about one deployment that is not in heimdall.yaml:
// who it belongs to, which commit and generation it is, where its images are,
// and how the target cluster is set up. The control plane (or the CLI) builds
// it; heimdall.yaml can never influence it (ADR 0006).
type Context struct {
	// Tenant is the tenant's slug: lowercase letters, digits and '-'.
	Tenant string
	// Repo is the GitHub repository as "owner/name".
	Repo string
	// PR is the pull request number.
	PR int
	// SHA is the full commit SHA (40 or 64 hex characters).
	SHA string
	// Generation is the deployment generation (ADR 0005), >= 1. Every object
	// is labelled with it, and per-generation objects (Jobs, the seed) carry
	// it in their names so a newer deployment never reuses an older one's.
	Generation int64
	// EnvironmentID identifies the environment across generations
	// (lowercase letters, digits and '-').
	EnvironmentID string
	// Owner is the GitHub login of the PR author. Metadata only.
	Owner string
	// ExpiresAt is when the environment expires. Metadata only: the TTL is
	// enforced by the controller, never by reading the label back.
	ExpiresAt time.Time
	// URLSuffix makes preview hostnames hard to guess: 4-8 lowercase letters
	// or digits, random per environment and stable across its generations.
	URLSuffix string

	// Images maps each service and worker name to the image to run, pinned by
	// digest (repo[:tag]@sha256:<64 hex>). A workload whose `image:` is
	// already pinned in heimdall.yaml may be omitted.
	Images map[string]string
	// Seed is the content of dependencies.postgres.seed. Required exactly
	// when the config declares a seed file.
	Seed []byte
	// Credentials, when set, are rendered into the heimdall-credentials
	// Secret. When nil the Secret is not rendered and pods reference one the
	// caller manages; `heimdall render` leaves it out unless asked.
	Credentials *Credentials

	// Policy is the tenant policy the config was loaded with. Its limits size
	// the namespace quota and limit range.
	Policy config.Policy
	// Platform describes the target cluster.
	Platform Platform
}

// Platform is cluster-level setup supplied by the platform operator, never by
// a pull request. Zero values select the documented defaults.
type Platform struct {
	// BaseDomain is the DNS zone covered by the cluster's wildcard
	// certificate; preview hosts are single labels directly beneath it.
	// Required.
	BaseDomain string
	// URLScheme is "https" (default) or "http" (local clusters only).
	URLScheme string
	// URLPort is added to public URLs when non-zero (local port mappings).
	URLPort int
	// Gateway is the shared Gateway (Gateway API) routes attach to.
	// Default: heimdall-gateway/heimdall.
	Gateway GatewayRef
	// IngressPeers may reach public services: the gateway's data plane.
	// Default: every pod in the Gateway's namespace.
	IngressPeers []networkingv1.NetworkPolicyPeer
	// DNSPeers is where pods resolve names. Default: kube-dns in kube-system.
	DNSPeers []networkingv1.NetworkPolicyPeer
	// EgressCIDRs are destinations outside the cluster previews may reach.
	// Empty (the default) allows none. Cloud metadata addresses are always
	// excluded, even from 0.0.0.0/0.
	EgressCIDRs []string
	// StorageClassName for the database volume. Empty: the cluster default.
	StorageClassName string
	// NodeSelector and Tolerations pin previews to a dedicated node pool.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	// RuntimeClassName selects a sandboxed runtime (for example gVisor).
	RuntimeClassName string
	// ImageMirror, when set, replaces the registry of Heimdall's own images
	// (dependencies, toolbox), for example a pull-through cache. Images stay
	// pinned by digest, so the content cannot change.
	ImageMirror string
	// WritableRootFilesystem relaxes readOnlyRootFilesystem for application
	// containers whose images cannot run without it. Heimdall's own
	// containers stay read-only.
	WritableRootFilesystem bool
}

// GatewayRef names a Gateway API Gateway, optionally one of its listeners.
type GatewayRef struct {
	Namespace   string
	Name        string
	SectionName string
}

// Credentials are the generated per-environment passwords. They are
// alphanumeric so they can be embedded in connection URLs unescaped.
type Credentials struct {
	PostgresSuperuser string
	PostgresApp       string
	Redis             string
	RabbitMQ          string
}

// GenerateCredentials returns fresh random credentials: 26 base32 characters
// (130 bits) each, from crypto/rand.
func GenerateCredentials() *Credentials {
	return &Credentials{
		PostgresSuperuser: rand.Text(),
		PostgresApp:       rand.Text(),
		Redis:             rand.Text(),
		RabbitMQ:          rand.Text(),
	}
}

// Defaults for zero-valued Platform fields.
const (
	DefaultGatewayNamespace = "heimdall-gateway"
	DefaultGatewayName      = "heimdall"
)

// MaxSeedBytes bounds the seed file. It travels in a ConfigMap, and the
// Kubernetes API rejects objects much over 1 MiB. Larger fixtures will travel
// in the OCI bundle fetched by the agent (P3, ADR 0004).
const MaxSeedBytes = 768 << 10

const maxPR = 99_999_999

var (
	tenantRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	ownerRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`)
	shaRE      = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	suffixRE   = regexp.MustCompile(`^[a-z0-9]{4,8}$`)
	passwordRE = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
	mirrorRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*$`)
)

// metadataPrefixes are cloud instance-metadata ranges. Previews must never
// reach them (credential theft), whatever the egress allowlist says.
var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),    // IPv4 link-local: AWS/GCP/Azure IMDS, ECS task metadata
	netip.MustParsePrefix("fd00:ec2::254/128"), // AWS IMDS over IPv6
}

// withDefaults returns p with zero values replaced by defaults.
func (p Platform) withDefaults() Platform {
	if p.URLScheme == "" {
		p.URLScheme = "https"
	}
	if p.Gateway.Namespace == "" {
		p.Gateway.Namespace = DefaultGatewayNamespace
	}
	if p.Gateway.Name == "" {
		p.Gateway.Name = DefaultGatewayName
	}
	// Empty, not just nil: a NetworkPolicy rule whose peer list is empty
	// matches every peer, which would open public services (and DNS egress)
	// to the whole cluster.
	if len(p.IngressPeers) == 0 {
		p.IngressPeers = []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				corev1.LabelMetadataName: p.Gateway.Namespace,
			}},
		}}
	}
	if len(p.DNSPeers) == 0 {
		p.DNSPeers = []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}}
	}
	return p
}

// validate checks every field and reports all problems at once.
func (c *Context) validate(cfg *config.Config) Errors {
	var errs Errors
	bad := func(field, format string, args ...any) {
		errs.add(CodeContextInvalid, "Context."+field, format, args...)
	}

	if !tenantRE.MatchString(c.Tenant) {
		bad("Tenant", "%q must be 1-40 lowercase letters, digits or '-'", c.Tenant)
	}
	if !repoRE.MatchString(c.Repo) || strings.HasSuffix(c.Repo, "/.") || strings.HasSuffix(c.Repo, "/..") {
		bad("Repo", "%q must be a GitHub repository as owner/name", c.Repo)
	}
	if c.PR < 1 || c.PR > maxPR {
		bad("PR", "%d must be between 1 and %d", c.PR, maxPR)
	}
	if !shaRE.MatchString(c.SHA) {
		bad("SHA", "%q must be a full lowercase commit SHA", c.SHA)
	}
	if c.Generation < 1 {
		bad("Generation", "%d must be at least 1", c.Generation)
	}
	if errs := validation.IsDNS1123Label(c.EnvironmentID); len(errs) > 0 {
		bad("EnvironmentID", "%q: %s", c.EnvironmentID, strings.Join(errs, "; "))
	}
	if !ownerRE.MatchString(c.Owner) {
		bad("Owner", "%q must be a GitHub login", c.Owner)
	}
	if c.ExpiresAt.IsZero() {
		bad("ExpiresAt", "is required")
	}
	if !suffixRE.MatchString(c.URLSuffix) {
		bad("URLSuffix", "%q must be 4-8 lowercase letters or digits", c.URLSuffix)
	}
	if c.Policy.MaxTotalCPUMilli <= 0 || c.Policy.MaxTotalMemoryMi <= 0 ||
		c.Policy.MaxContainerCPUMilli <= 0 || c.Policy.MaxContainerMemoryMi <= 0 {
		bad("Policy", "limits are required (use the policy the config was loaded with)")
	}
	if cr := c.Credentials; cr != nil {
		for _, f := range []struct{ name, value string }{
			{"PostgresSuperuser", cr.PostgresSuperuser}, {"PostgresApp", cr.PostgresApp},
			{"Redis", cr.Redis}, {"RabbitMQ", cr.RabbitMQ},
		} {
			if !passwordRE.MatchString(f.value) {
				errs.add(CodeCredentialsInvalid, "Context.Credentials."+f.name, "must be 16-128 letters or digits")
			}
		}
	}

	switch seed := cfg.Dependencies.Postgres != nil && cfg.Dependencies.Postgres.Seed != ""; {
	case seed && len(c.Seed) == 0:
		errs.add(CodeSeedMissing, "Context.Seed", "the config declares a seed file (%s) but no content was provided",
			cfg.Dependencies.Postgres.Seed)
	case !seed && len(c.Seed) > 0:
		errs.add(CodeSeedUnexpected, "Context.Seed", "seed content was provided but the config declares no seed file")
	case len(c.Seed) > MaxSeedBytes:
		errs.add(CodeSeedTooLarge, "Context.Seed", "seed file is %d KiB; the limit is %d KiB",
			len(c.Seed)>>10, MaxSeedBytes>>10)
	}

	errs = append(errs, c.Platform.validate()...)
	return errs
}

// ValidatePlatform checks platform settings (after defaults) exactly as Render
// will, so a misconfigured agent fails at start-up rather than on first use.
func ValidatePlatform(p Platform) error {
	return p.withDefaults().validate().err()
}

func (p Platform) validate() Errors {
	var errs Errors
	bad := func(field, format string, args ...any) {
		errs.add(CodeContextInvalid, "Context.Platform."+field, format, args...)
	}
	switch d := p.BaseDomain; {
	case d == "":
		bad("BaseDomain", "is required")
	case len(validation.IsDNS1123Subdomain(d)) > 0 || !strings.Contains(d, ".") || len(d) > 253-64:
		bad("BaseDomain", "%q must be a lowercase DNS name such as preview.example.com", d)
	}
	if p.URLScheme != "http" && p.URLScheme != "https" {
		bad("URLScheme", "%q must be http or https", p.URLScheme)
	}
	if p.URLPort < 0 || p.URLPort > 65535 {
		bad("URLPort", "%d is out of range", p.URLPort)
	}
	if len(validation.IsDNS1123Label(p.Gateway.Namespace)) > 0 {
		bad("Gateway.Namespace", "%q is not a valid namespace name", p.Gateway.Namespace)
	}
	if len(validation.IsDNS1123Subdomain(p.Gateway.Name)) > 0 {
		bad("Gateway.Name", "%q is not a valid object name", p.Gateway.Name)
	}
	if s := p.Gateway.SectionName; s != "" && len(validation.IsDNS1123Subdomain(s)) > 0 {
		bad("Gateway.SectionName", "%q is not a valid listener name", s)
	}
	for i, cidr := range p.EgressCIDRs {
		pfx, err := netip.ParsePrefix(cidr)
		switch {
		case err != nil:
			bad(fmt.Sprintf("EgressCIDRs[%d]", i), "%q is not a CIDR", cidr)
		case pfx != pfx.Masked():
			bad(fmt.Sprintf("EgressCIDRs[%d]", i), "%q has host bits set; use %s", cidr, pfx.Masked())
		case slices.ContainsFunc(metadataPrefixes, func(m netip.Prefix) bool { return m.Bits() <= pfx.Bits() && m.Overlaps(pfx) }):
			bad(fmt.Sprintf("EgressCIDRs[%d]", i), "%q is inside a cloud metadata range and can never be allowed", cidr)
		}
	}
	if s := p.StorageClassName; s != "" && len(validation.IsDNS1123Subdomain(s)) > 0 {
		bad("StorageClassName", "%q is not a valid storage class name", s)
	}
	if s := p.RuntimeClassName; s != "" && len(validation.IsDNS1123Subdomain(s)) > 0 {
		bad("RuntimeClassName", "%q is not a valid runtime class name", s)
	}
	if p.ImageMirror != "" && !mirrorRE.MatchString(p.ImageMirror) {
		bad("ImageMirror", "%q must be a registry host with an optional path, such as mirror.example.com/hub", p.ImageMirror)
	}
	return errs
}
