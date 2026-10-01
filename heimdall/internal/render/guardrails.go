package render

import (
	"fmt"
	"net/netip"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// Names of guardrail objects.
const (
	quotaName           = "heimdall-quota"
	limitRangeName      = "heimdall-limits"
	policyDefaultDeny   = "heimdall-default-deny"
	policySameNS        = "heimdall-allow-same-namespace"
	policyDNS           = "heimdall-allow-dns"
	policyEgress        = "heimdall-allow-egress"
	policyGatewayPrefix = "heimdall-allow-gateway"
)

// Pod Security Admission labels: the namespace itself rejects any pod that is
// not "restricted", even one the renderer did not produce.
const (
	psaEnforce        = "pod-security.kubernetes.io/enforce"
	psaEnforceVersion = "pod-security.kubernetes.io/enforce-version"
	psaAudit          = "pod-security.kubernetes.io/audit"
	psaWarn           = "pod-security.kubernetes.io/warn"
	psaRestricted     = "restricted"
)

var (
	typeNamespace      = metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}
	typeServiceAccount = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
	typeSecret         = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	typeConfigMap      = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	typeResourceQuota  = metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"}
	typeLimitRange     = metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"}
	typeService        = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	typeNetworkPolicy  = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
	typeDeployment     = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	typeStatefulSet    = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}
	typeJob            = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
	typeHTTPRoute      = metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"}
)

// guardrails creates the namespace and everything that constrains what runs
// in it, before anything runs. later are the other stages, used to size the
// quota for their Jobs.
func (b *builder) guardrails(later ...Stage) Stage {
	objs := []Object{b.namespace(), b.serviceAccount()}
	if s := b.credentials(); s != nil {
		objs = append(objs, s)
	}
	objs = append(objs, b.quota(later), b.limitRange())
	objs = append(objs, b.networkPolicies()...)
	return Stage{Name: StageGuardrails, Steps: []Step{{Name: StepSetup, Objects: objs}}}
}

func (b *builder) namespace() *corev1.Namespace {
	labels := b.objectLabels(StageGuardrails, componentGuardrail)
	labels[LabelPreview] = "true"
	labels[psaEnforce] = psaRestricted
	labels[psaEnforceVersion] = "latest"
	labels[psaAudit] = psaRestricted
	labels[psaWarn] = psaRestricted
	ann := b.annotations()
	ann[AnnotationRepo] = b.ctx.Repo
	ann[AnnotationExpiresAt] = rfc3339(b.ctx.ExpiresAt)
	ann[AnnotationVisibility] = b.cfg.Preview.Visibility
	if p := b.cfg.PrimaryService(); p != "" {
		ann[AnnotationURL] = b.urls[p]
	}
	return &corev1.Namespace{
		TypeMeta:   typeNamespace,
		ObjectMeta: metav1.ObjectMeta{Name: b.ns, Labels: labels, Annotations: ann},
	}
}

func (b *builder) serviceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:                     typeServiceAccount,
		ObjectMeta:                   b.meta(WorkloadServiceAccount, StageGuardrails, componentGuardrail),
		AutomountServiceAccountToken: ptr(false),
	}
}

// Database and role names inside the environment's PostgreSQL.
const (
	dbApp      = "app"          // live database, used by services and workers
	dbBaseline = "app_baseline" // migrated and seeded template that reset restores
	dbRole     = "app"          // owns both databases; not a superuser
)

// credentials renders generated passwords and the connection URLs built from
// them. Passwords are alphanumeric, so the URLs need no escaping.
func (b *builder) credentials() *corev1.Secret {
	c := b.ctx.Credentials
	if c == nil || len(b.cfg.EnabledDependencies()) == 0 {
		return nil
	}
	data := map[string][]byte{}
	if b.cfg.DependencyEnabled(config.DepPostgres) {
		url := func(db string) []byte {
			return fmt.Appendf(nil, "postgres://%s:%s@%s:5432/%s?sslmode=disable", dbRole, c.PostgresApp, config.DepPostgres, db)
		}
		data[keyPostgresSuperuserPassword] = []byte(c.PostgresSuperuser)
		data[keyPostgresAppPassword] = []byte(c.PostgresApp)
		data[keyDatabaseURL] = url(dbApp)
		data[keyDatabaseURLBaseline] = url(dbBaseline)
	}
	if b.cfg.DependencyEnabled(config.DepRedis) {
		data[keyRedisPassword] = []byte(c.Redis)
		data[keyRedisURL] = fmt.Appendf(nil, "redis://:%s@%s:6379/0", c.Redis, config.DepRedis)
	}
	if b.cfg.DependencyEnabled(config.DepRabbitMQ) {
		data[keyRabbitMQPassword] = []byte(c.RabbitMQ)
		// %2F is the default vhost "/", spelled out as the AMQP URI spec requires.
		data[keyAMQPURL] = fmt.Appendf(nil, "amqp://%s:%s@%s:5672/%%2F", rabbitMQUser, c.RabbitMQ, config.DepRabbitMQ)
	}
	return &corev1.Secret{
		TypeMeta:   typeSecret,
		ObjectMeta: b.meta(CredentialsSecret, StageGuardrails, componentGuardrail),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// quota caps the namespace at the tenant's ceilings, plus room for the
// largest step of concurrently running Jobs (migrations, smoke tests), and
// forbids LoadBalancer and NodePort services outright: previews are only
// reachable through the shared gateway.
func (b *builder) quota(stages []Stage) *corev1.ResourceQuota {
	jobCPU, jobMem, jobPods := jobFootprint(stages)
	l := b.ctx.Policy.Limits
	cpu, mem := milliQuantity(l.MaxTotalCPUMilli), mebiQuantity(l.MaxTotalMemoryMi)
	cpu.Add(jobCPU)
	mem.Add(jobMem)
	pods := l.MaxServices + l.MaxWorkers*config.MaxWorkerReplicas + len(b.cfg.EnabledDependencies()) + jobPods

	pvcs, storage := 0, resource.MustParse("0")
	if b.cfg.DependencyEnabled(config.DepPostgres) {
		pvcs, storage = 1, mebiQuantity(l.MaxPostgresStorageMi)
	}
	return &corev1.ResourceQuota{
		TypeMeta:   typeResourceQuota,
		ObjectMeta: b.meta(quotaName, StageGuardrails, componentGuardrail),
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU:              cpu,
			corev1.ResourceLimitsMemory:           mem,
			corev1.ResourcePods:                   resource.MustParse(strconv.Itoa(pods)),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse(strconv.Itoa(pvcs)),
			corev1.ResourceRequestsStorage:        storage,
			corev1.ResourceServicesLoadBalancers:  resource.MustParse("0"),
			corev1.ResourceServicesNodePorts:      resource.MustParse("0"),
		}},
	}
}

// jobFootprint returns the largest per-step totals of Job container limits and
// Job count. Steps run one after another, so only one step's Jobs coexist.
func jobFootprint(stages []Stage) (cpu, mem resource.Quantity, pods int) {
	cpu, mem = resource.MustParse("0"), resource.MustParse("0")
	for _, s := range stages {
		for _, st := range s.Steps {
			c, m, n := resource.MustParse("0"), resource.MustParse("0"), 0
			for _, o := range st.Objects {
				job, ok := o.(*batchv1.Job)
				if !ok {
					continue
				}
				n++
				for _, ctr := range job.Spec.Template.Spec.Containers {
					c.Add(ctr.Resources.Limits[corev1.ResourceCPU])
					m.Add(ctr.Resources.Limits[corev1.ResourceMemory])
				}
			}
			if c.Cmp(cpu) > 0 {
				cpu = c
			}
			if m.Cmp(mem) > 0 {
				mem = m
			}
			pods = max(pods, n)
		}
	}
	return cpu, mem, pods
}

// limitRange enforces the per-container ceilings at admission and caps memory
// over-commitment (limit/request) at the policy's ratio, so the scheduler's
// view of memory stays honest. Defaults cover any container that omits
// resources; the renderer never does.
func (b *builder) limitRange() *corev1.LimitRange {
	l := b.ctx.Policy.Limits
	maxCPU, maxMem := milliQuantity(l.MaxContainerCPUMilli), mebiQuantity(l.MaxContainerMemoryMi)
	def := config.ResolveResources(config.Resources{}, config.SizeMedium, b.ctx.Policy)
	defReq, _ := requirements(def)
	clamp := func(q, ceiling resource.Quantity) resource.Quantity {
		if q.Cmp(ceiling) > 0 {
			return ceiling
		}
		return q
	}
	defCPU := clamp(defReq.Limits[corev1.ResourceCPU], maxCPU)
	defMem := clamp(defReq.Limits[corev1.ResourceMemory], maxMem)
	container := corev1.LimitRangeItem{
		Type:           corev1.LimitTypeContainer,
		Max:            corev1.ResourceList{corev1.ResourceCPU: maxCPU, corev1.ResourceMemory: maxMem},
		Default:        corev1.ResourceList{corev1.ResourceCPU: defCPU, corev1.ResourceMemory: defMem},
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: clamp(defReq.Requests[corev1.ResourceCPU], defCPU), corev1.ResourceMemory: clamp(defReq.Requests[corev1.ResourceMemory], defMem)},
	}
	if pct := l.MinMemoryRequestPercent; pct > 0 {
		// Round up so every request the validator accepts is admitted.
		ratio := (100_000 + pct - 1) / pct
		container.MaxLimitRequestRatio = corev1.ResourceList{corev1.ResourceMemory: milliQuantity(ratio)}
	}
	items := []corev1.LimitRangeItem{container}
	if b.cfg.DependencyEnabled(config.DepPostgres) {
		items = append(items, corev1.LimitRangeItem{
			Type: corev1.LimitTypePersistentVolumeClaim,
			Max:  corev1.ResourceList{corev1.ResourceStorage: mebiQuantity(l.MaxPostgresStorageMi)},
		})
	}
	return &corev1.LimitRange{
		TypeMeta:   typeLimitRange,
		ObjectMeta: b.meta(limitRangeName, StageGuardrails, componentGuardrail),
		Spec:       corev1.LimitRangeSpec{Limits: items},
	}
}

// networkPolicies deny all traffic, then allow exactly: traffic within the
// namespace, DNS, the gateway to each public service's port, and the
// platform's egress allowlist. Nothing else can reach in or out, including
// other previews, the Kubernetes API and cloud metadata endpoints.
func (b *builder) networkPolicies() []Object {
	all := metav1.LabelSelector{}
	sameNS := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	objs := []Object{
		b.netpol(policyDefaultDeny, networkingv1.NetworkPolicySpec{
			PodSelector: all,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		}),
		b.netpol(policySameNS, networkingv1.NetworkPolicySpec{
			PodSelector: all,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: sameNS}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: sameNS}},
		}),
		b.netpol(policyDNS, networkingv1.NetworkPolicySpec{
			PodSelector: all,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    b.platform.DNSPeers,
				Ports: []networkingv1.NetworkPolicyPort{port(corev1.ProtocolUDP, 53), port(corev1.ProtocolTCP, 53)},
			}},
		}),
	}
	for _, name := range sortedKeys(b.urls) {
		objs = append(objs, b.netpol(objectName(policyGatewayPrefix, name), networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector(name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  b.platform.IngressPeers,
				Ports: []networkingv1.NetworkPolicyPort{port(corev1.ProtocolTCP, b.cfg.Services[name].Port)},
			}},
		}))
	}
	if len(b.platform.EgressCIDRs) > 0 {
		var peers []networkingv1.NetworkPolicyPeer
		for _, cidr := range b.platform.EgressCIDRs {
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: metadataExcepts(cidr)}})
		}
		objs = append(objs, b.netpol(policyEgress, networkingv1.NetworkPolicySpec{
			PodSelector: all,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		}))
	}
	return objs
}

func (b *builder) netpol(name string, spec networkingv1.NetworkPolicySpec) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta:   typeNetworkPolicy,
		ObjectMeta: b.meta(name, StageGuardrails, componentGuardrail),
		Spec:       spec,
	}
}

func port(proto corev1.Protocol, n int) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(int32(n))
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

// metadataExcepts returns the metadata ranges strictly inside cidr, so an
// allowlist such as 0.0.0.0/0 still cannot reach instance credentials.
// (Ranges that contain cidr are rejected by Context validation.)
func metadataExcepts(cidr string) []string {
	pfx := netip.MustParsePrefix(cidr)
	var out []string
	for _, m := range metadataPrefixes {
		if pfx.Bits() < m.Bits() && pfx.Overlaps(m) {
			out = append(out, m.String())
		}
	}
	return out
}
