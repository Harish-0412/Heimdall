package render

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// The P1 exit criterion: no pod is ever rendered with privileged settings,
// host access, a mounted service-account token, or an image that is not
// pinned by digest. checkPlan is applied to every golden scenario and to every
// randomly generated config in the property test.

var pinnedImageRE = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)

// requiredLabels must be on every object (ADR 0005 metadata).
var requiredLabels = []string{
	LabelTenant, LabelRepo, LabelPR, LabelEnv, LabelGeneration, LabelOwner, LabelExpires, LabelStage, labelManagedBy,
}

// checkPlan returns every violation of the platform's invariants in p.
func checkPlan(p *Plan, ctx Context) []string {
	var v []string
	bad := func(o Object, format string, args ...any) {
		v = append(v, fmt.Sprintf("%s/%s: %s", kindOf(o), o.GetName(), fmt.Sprintf(format, args...)))
	}
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 || !strings.HasPrefix(p.Namespace, NamespacePrefix) {
		v = append(v, fmt.Sprintf("namespace %q is invalid: %v", p.Namespace, errs))
	}
	secrets := credentialValues(ctx)
	for _, o := range p.Objects() {
		if kindOf(o) == "" || o.GetObjectKind().GroupVersionKind().Version == "" {
			bad(o, "TypeMeta not set")
		}
		if kindOf(o) != "Namespace" && o.GetNamespace() != p.Namespace {
			bad(o, "in namespace %q, not the environment's %q", o.GetNamespace(), p.Namespace)
		}
		if errs := validation.IsDNS1123Label(o.GetName()); len(errs) > 0 {
			bad(o, "name is not a DNS label: %v", errs)
		}
		for _, k := range requiredLabels {
			if o.GetLabels()[k] == "" {
				bad(o, "missing label %s", k)
			}
		}
		for k, val := range o.GetLabels() {
			if errs := append(validation.IsQualifiedName(k), validation.IsValidLabelValue(val)...); len(errs) > 0 {
				bad(o, "invalid label %s=%q: %v", k, val, errs)
			}
		}

		switch o := o.(type) {
		case *corev1.Namespace:
			if o.Labels[psaEnforce] != psaRestricted || o.Labels[LabelPreview] != "true" {
				bad(o, "namespace must enforce Pod Security restricted and carry %s", LabelPreview)
			}
		case *corev1.ServiceAccount:
			if o.AutomountServiceAccountToken == nil || *o.AutomountServiceAccountToken {
				bad(o, "service account must not automount tokens")
			}
		case *corev1.Service:
			if o.Spec.Type != corev1.ServiceTypeClusterIP || len(o.Spec.ExternalIPs) > 0 {
				bad(o, "only plain ClusterIP services are allowed, got %s %v", o.Spec.Type, o.Spec.ExternalIPs)
			}
		case *corev1.Secret:
			if o.Name != CredentialsSecret {
				bad(o, "unexpected secret; app secrets are delivered by the platform")
			}
		case *gatewayv1.HTTPRoute:
			for _, pr := range o.Spec.ParentRefs {
				if string(pr.Name) != ctx.Platform.withDefaults().Gateway.Name || pr.Namespace == nil ||
					string(*pr.Namespace) != ctx.Platform.withDefaults().Gateway.Namespace {
					bad(o, "route attaches to an unexpected gateway %v", pr)
				}
			}
			for _, h := range o.Spec.Hostnames {
				label, zone, _ := strings.Cut(string(h), ".")
				if zone != ctx.Platform.BaseDomain || len(validation.IsDNS1123Label(label)) > 0 {
					bad(o, "hostname %s is not a single label under %s", h, ctx.Platform.BaseDomain)
				}
			}
		case *appsv1.Deployment:
			v = append(v, checkPod(o, o.Spec.Template.Spec, secrets, ctx)...)
		case *appsv1.StatefulSet:
			v = append(v, checkPod(o, o.Spec.Template.Spec, secrets, ctx)...)
		case *batchv1.Job:
			v = append(v, checkPod(o, o.Spec.Template.Spec, secrets, ctx)...)
			if o.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever || o.Spec.ActiveDeadlineSeconds == nil {
				bad(o, "jobs must not restart in place and must have a deadline")
			}
		}
	}
	return v
}

func checkPod(o Object, spec corev1.PodSpec, secrets []string, ctx Context) []string {
	var v []string
	bad := func(format string, args ...any) {
		v = append(v, fmt.Sprintf("%s/%s: %s", kindOf(o), o.GetName(), fmt.Sprintf(format, args...)))
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		bad("service-account token is mounted")
	}
	if spec.ServiceAccountName != WorkloadServiceAccount {
		bad("runs as service account %q", spec.ServiceAccountName)
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		bad("uses host namespaces")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		bad("service links are enabled")
	}
	psc := spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.RunAsUser == nil || *psc.RunAsUser == 0 ||
		psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		bad("pod security context must be non-root with seccomp RuntimeDefault: %+v", psc)
	}
	for _, vol := range spec.Volumes {
		switch {
		case vol.EmptyDir != nil:
			if vol.EmptyDir.SizeLimit == nil {
				bad("emptyDir %s has no size limit", vol.Name)
			}
		case vol.ConfigMap != nil, vol.PersistentVolumeClaim != nil:
		default:
			bad("volume %s has a forbidden type (host path, projected token, ...)", vol.Name)
		}
	}
	if len(spec.InitContainers)+len(spec.EphemeralContainers) > 0 {
		bad("unexpected init or ephemeral containers")
	}
	for _, c := range spec.Containers {
		if !pinnedImageRE.MatchString(c.Image) || imageTag(c.Image) == "latest" {
			bad("container %s image %q is not pinned by digest", c.Name, c.Image)
		}
		sc := c.SecurityContext
		readOnlyExpected := !ctx.Platform.WritableRootFilesystem || isPlatformImage(c.Image)
		switch {
		case sc == nil:
			bad("container %s has no security context", c.Name)
		case sc.Privileged == nil || *sc.Privileged:
			bad("container %s may be privileged", c.Name)
		case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
			bad("container %s allows privilege escalation", c.Name)
		case sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) > 0:
			bad("container %s does not drop all capabilities", c.Name)
		case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot:
			bad("container %s may run as root", c.Name)
		case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
			bad("container %s has no RuntimeDefault seccomp profile", c.Name)
		case readOnlyExpected && (sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem):
			bad("container %s has a writable root filesystem", c.Name)
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 || p.HostIP != "" {
				bad("container %s binds a host port", c.Name)
			}
		}
		for _, m := range c.VolumeMounts {
			if strings.HasPrefix(m.MountPath, "/var/run/secrets") {
				bad("container %s mounts %s", c.Name, m.MountPath)
			}
		}
		lim, req := c.Resources.Limits, c.Resources.Requests
		for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			l, okL := lim[r]
			q, okQ := req[r]
			if !okL || !okQ || q.Cmp(l) > 0 {
				bad("container %s %s request/limit missing or inverted: %v/%v", c.Name, r, req, lim)
			}
		}
		if mem, memReq := lim.Memory().Value(), req.Memory().Value(); memReq > 0 && mem > 2*memReq {
			bad("container %s over-commits memory: limit %d > 2x request %d", c.Name, mem, memReq)
		}
		for _, e := range c.Env {
			for _, s := range secrets {
				if strings.Contains(e.Value, s) {
					bad("container %s has a credential in plain env %s", c.Name, e.Name)
				}
			}
		}
	}
	return v
}

func isPlatformImage(ref string) bool {
	return strings.Contains(ref, "/library/") || strings.Contains(ref, "/curl/curl:")
}

func credentialValues(ctx Context) []string {
	if ctx.Credentials == nil {
		return nil
	}
	c := ctx.Credentials
	return []string{c.PostgresSuperuser, c.PostgresApp, c.Redis, c.RabbitMQ}
}

func kindOf(o Object) string { return o.GetObjectKind().GroupVersionKind().Kind }

func TestSecurityDefaults_GoldenScenarios(t *testing.T) {
	for _, sc := range goldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			probe := Context{Policy: config.DefaultPolicy()}
			if sc.edit != nil {
				sc.edit(&probe)
			}
			cfg := loadFile(t, sc.config, probe.Policy)
			ctx := testContext(t, cfg)
			if sc.edit != nil {
				sc.edit(&ctx)
			}
			for _, violation := range checkPlan(mustRender(t, cfg, ctx), ctx) {
				t.Error(violation)
			}
		})
	}
}

// TestSecurityChecker_DetectsViolations proves the checker is not vacuous:
// each tampering below must be reported.
func TestSecurityChecker_DetectsViolations(t *testing.T) {
	tamper := map[string]func(*corev1.PodSpec){
		"privileged":           func(s *corev1.PodSpec) { s.Containers[0].SecurityContext.Privileged = ptr(true) },
		"privilege escalation": func(s *corev1.PodSpec) { s.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr(true) },
		"capability added": func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		},
		"token mounted":    func(s *corev1.PodSpec) { s.AutomountServiceAccountToken = ptr(true) },
		"host network":     func(s *corev1.PodSpec) { s.HostNetwork = true },
		"root user":        func(s *corev1.PodSpec) { s.SecurityContext.RunAsUser = ptr(int64(0)) },
		"tag-only image":   func(s *corev1.PodSpec) { s.Containers[0].Image = "nginx:1.27" },
		"latest image":     func(s *corev1.PodSpec) { s.Containers[0].Image = "nginx:latest@sha256:" + strings.Repeat("a", 64) },
		"writable root fs": func(s *corev1.PodSpec) { s.Containers[0].SecurityContext.ReadOnlyRootFilesystem = ptr(false) },
		"host path": func(s *corev1.PodSpec) {
			s.Volumes = append(s.Volumes, corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"host port": func(s *corev1.PodSpec) {
			s.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}}
		},
		"literal password": func(s *corev1.PodSpec) {
			s.Containers[0].Env = append(s.Containers[0].Env, corev1.EnvVar{Name: "X", Value: testCredentials.Redis})
		},
		"memory over-commit": func(s *corev1.PodSpec) {
			s.Containers[0].Resources.Requests[corev1.ResourceMemory] = mebiQuantity(16)
		},
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			plan, ctx := shopflowPlan(t)
			dep := find[*appsv1.Deployment](plan, "api")
			mutate(&dep.Spec.Template.Spec)
			if v := checkPlan(plan, ctx); len(v) == 0 {
				t.Fatal("checker did not report the violation")
			}
		})
	}
	t.Run("clean plan has no violations", func(t *testing.T) {
		plan, ctx := shopflowPlan(t)
		if v := checkPlan(plan, ctx); len(v) > 0 {
			t.Fatalf("unexpected violations: %v", v)
		}
	})
}
