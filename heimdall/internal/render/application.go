package render

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const (
	portName        = "http"
	tcpProbeDelay   = 5
	gatewayKind     = "Gateway"
	defaultReplicas = 1
)

// application deploys services and workers in waves: a workload starts only
// after every service it depends on is ready. Wave 1 holds workloads that
// depend on no service; dependencies on postgres/redis/rabbitmq are already
// satisfied by the earlier stages.
func (b *builder) application() Stage {
	stage := Stage{Name: StageApplication}
	for i, names := range b.waves() {
		var objs []Object
		for _, name := range names {
			if s, ok := b.cfg.Services[name]; ok {
				objs = append(objs, b.appService(name, s), b.serviceDeployment(name, s))
				if s.Public {
					objs = append(objs, b.route(name, s))
				}
				continue
			}
			objs = append(objs, b.workerDeployment(name, b.cfg.Workers[name]))
		}
		stage.Steps = append(stage.Steps, Step{Name: fmt.Sprintf("wave-%d", i+1), Objects: objs})
	}
	return stage
}

// waves orders workloads by dependency depth on services. Load rejects
// cycles, so the recursion terminates.
func (b *builder) waves() [][]string {
	depth := map[string]int{}
	var visit func(string) int
	visit = func(name string) int {
		if d, ok := depth[name]; ok {
			return d
		}
		deps := b.cfg.Workers[name].DependsOn
		if s, ok := b.cfg.Services[name]; ok {
			deps = s.DependsOn
		}
		d := 1
		for _, dep := range deps {
			if _, isService := b.cfg.Services[dep]; isService {
				d = max(d, visit(dep)+1)
			}
		}
		depth[name] = d
		return d
	}
	var waves [][]string
	for _, names := range [][]string{sortedKeys(b.cfg.Services), sortedKeys(b.cfg.Workers)} {
		for _, name := range names {
			d := visit(name)
			for len(waves) < d {
				waves = append(waves, nil)
			}
			waves[d-1] = append(waves[d-1], name)
		}
	}
	return waves
}

func (b *builder) workloadResources(field string, r config.Resources) corev1.ResourceRequirements {
	req, err := requirements(r)
	if err != nil {
		b.errs.add(CodeResourcesInvalid, field, "%v", err)
	}
	return req
}

func (b *builder) appService(name string, s config.Service) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   typeService,
		ObjectMeta: b.meta(name, StageApplication, componentService),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector(name),
			Ports: []corev1.ServicePort{{
				Name: portName, Port: int32(s.Port), TargetPort: intstr.FromString(portName), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

func (b *builder) serviceDeployment(name string, s config.Service) *appsv1.Deployment {
	c := container(name, b.images[name], !b.platform.WritableRootFilesystem)
	c.Ports = []corev1.ContainerPort{{Name: portName, ContainerPort: int32(s.Port), Protocol: corev1.ProtocolTCP}}
	c.Env = b.workloadEnv("services."+name, s.Port, s.Env, s.Secrets, liveDatabase)
	c.Resources = b.workloadResources("services."+name, s.Resources)
	c.VolumeMounts = []corev1.VolumeMount{mount("tmp", "/tmp")}
	// Readiness only: a liveness probe guessed for an unknown app restarts slow
	// starters in a loop. A crashing app is diagnosed from its restarts (P4).
	if h := s.Health; h != nil {
		c.ReadinessProbe = probe(corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: h.Path, Port: intstr.FromString(portName),
		}}, int32(h.InitialDelay.Std().Seconds()))
	} else {
		c.ReadinessProbe = tcpProbe(portName, tcpProbeDelay)
	}
	return b.appDeployment(name, componentService, defaultReplicas, c)
}

func (b *builder) workerDeployment(name string, w config.Worker) *appsv1.Deployment {
	c := container(name, b.images[name], !b.platform.WritableRootFilesystem)
	c.Command = []string{"sh", "-c", w.Command}
	c.Env = b.workloadEnv("workers."+name, 0, w.Env, w.Secrets, liveDatabase)
	c.Resources = b.workloadResources("workers."+name, w.Resources)
	c.VolumeMounts = []corev1.VolumeMount{mount("tmp", "/tmp")}
	return b.appDeployment(name, componentWorker, w.Replicas, c)
}

// appDeployment replaces pods one at a time without surging, so a rollout
// never needs more than the environment's quota: a preview trades a few
// seconds of unavailability on push for a predictable footprint.
func (b *builder) appDeployment(name, component string, replicas int, c corev1.Container) *appsv1.Deployment {
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	return &appsv1.Deployment{
		TypeMeta:   typeDeployment,
		ObjectMeta: b.meta(name, StageApplication, component),
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(replicas)),
			Selector: &metav1.LabelSelector{MatchLabels: selector(name)},
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &zero, MaxUnavailable: &one},
			},
			RevisionHistoryLimit: ptr(int32(2)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: b.podLabels(name, component)},
				Spec: b.podSpec(appUID, appGID, corev1.RestartPolicyAlways,
					[]corev1.Volume{scratch("tmp", tmpSize)}, c),
			},
		},
	}
}

// route attaches a public service to the cluster's shared Gateway under its
// preview hostname. TLS terminates at the Gateway with one wildcard
// certificate for the whole cluster, which never enters preview namespaces
// (ADR 0008).
func (b *builder) route(name string, s config.Service) *gatewayv1.HTTPRoute {
	meta := b.meta(name, StageApplication, componentRoute)
	meta.Annotations[AnnotationURL] = b.urls[name]
	meta.Annotations[AnnotationVisibility] = b.cfg.Preview.Visibility

	parent := gatewayv1.ParentReference{
		Group:     ptr(gatewayv1.Group(gatewayv1.GroupName)),
		Kind:      ptr(gatewayv1.Kind(gatewayKind)),
		Namespace: ptr(gatewayv1.Namespace(b.platform.Gateway.Namespace)),
		Name:      gatewayv1.ObjectName(b.platform.Gateway.Name),
	}
	if sec := b.platform.Gateway.SectionName; sec != "" {
		parent.SectionName = ptr(gatewayv1.SectionName(sec))
	}
	backend := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
		Name: gatewayv1.ObjectName(name),
		Port: ptr(gatewayv1.PortNumber(s.Port)),
	}}}
	return &gatewayv1.HTTPRoute{
		TypeMeta:   typeHTTPRoute,
		ObjectMeta: meta,
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(b.hosts[name])},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{backend}}},
		},
	}
}
