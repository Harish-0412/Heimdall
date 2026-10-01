package render

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const (
	postgresPort = 5432
	redisPort    = 6379
	rabbitMQPort = 5672
	rabbitMQUser = "app"

	postgresDataDir = "/var/lib/postgresql/data"
	// PGDATA sits one level below the mount point: volume roots often hold
	// lost+found, which initdb refuses.
	postgresPGDATA = postgresDataDir + "/pgdata"
)

// dependencies starts the managed backing services and waits for them.
func (b *builder) dependencies() Stage {
	var objs []Object
	if p := b.cfg.Dependencies.Postgres; p != nil {
		objs = append(objs, b.dependencyService(config.DepPostgres, postgresPort, componentDatabase), b.postgres(p))
	}
	if r := b.cfg.Dependencies.Redis; r != nil {
		objs = append(objs, b.dependencyService(config.DepRedis, redisPort, componentCache), b.redis(r))
	}
	if r := b.cfg.Dependencies.RabbitMQ; r != nil {
		objs = append(objs, b.dependencyService(config.DepRabbitMQ, rabbitMQPort, componentBroker), b.rabbitMQ(r))
	}
	if len(objs) == 0 {
		return Stage{Name: StageDependencies}
	}
	return Stage{Name: StageDependencies, Steps: []Step{{Name: StepStart, Objects: objs}}}
}

func (b *builder) dependencyImage(dep, version string) catalogImage {
	img, ok := dependencyImages(dep)[version]
	if !ok {
		// Load only accepts supported versions and TestCatalog keeps the
		// catalog complete, so this is a build defect, not bad input.
		panic(fmt.Sprintf("render: no pinned image for %s %s", dep, version))
	}
	return img
}

func (b *builder) dependencyService(name string, port int, component string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   typeService,
		ObjectMeta: b.meta(name, StageDependencies, component),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector(name),
			Ports: []corev1.ServicePort{{
				Name: name, Port: int32(port), TargetPort: intstr.FromString(name), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

func (b *builder) dependencyResources(dep string) corev1.ResourceRequirements {
	r, err := requirements(config.DependencyResources(dep, b.ctx.Policy))
	if err != nil {
		panic(err)
	}
	return r
}

func probe(handler corev1.ProbeHandler, initialDelay int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        handler,
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       5,
		TimeoutSeconds:      3,
		FailureThreshold:    6,
	}
}

func tcpProbe(port string, initialDelay int32) *corev1.Probe {
	return probe(corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(port)}}, initialDelay)
}

// postgres is a single-replica StatefulSet so its data survives pod restarts.
// The claim is deleted with the StatefulSet: environments must leave nothing
// behind (P2's Destroy also verifies it).
func (b *builder) postgres(p *config.Postgres) *appsv1.StatefulSet {
	img := b.dependencyImage(config.DepPostgres, p.Version)
	c := container(config.DepPostgres, img.ref(b.platform.ImageMirror), true)
	c.Ports = []corev1.ContainerPort{{Name: config.DepPostgres, ContainerPort: postgresPort, Protocol: corev1.ProtocolTCP}}
	c.Env = []corev1.EnvVar{
		credential("POSTGRES_PASSWORD", keyPostgresSuperuserPassword),
		{Name: "PGDATA", Value: postgresPGDATA},
	}
	c.Resources = b.dependencyResources(config.DepPostgres)
	c.ReadinessProbe = probe(corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{"pg_isready", "--quiet", "--username=postgres", "--host=127.0.0.1"},
	}}, 5)
	c.VolumeMounts = []corev1.VolumeMount{mount("data", postgresDataDir), mount("run", "/var/run/postgresql"), mount("tmp", "/tmp")}

	storage, err := resource.ParseQuantity(p.Storage)
	if err != nil {
		panic(err) // validated by Load
	}
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: b.podLabels(config.DepPostgres, componentDatabase)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
		},
	}
	if b.platform.StorageClassName != "" {
		claim.Spec.StorageClassName = ptr(b.platform.StorageClassName)
	}

	return &appsv1.StatefulSet{
		TypeMeta:   typeStatefulSet,
		ObjectMeta: b.meta(config.DepPostgres, StageDependencies, componentDatabase),
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr(int32(1)),
			ServiceName: config.DepPostgres,
			Selector:    &metav1.LabelSelector{MatchLabels: selector(config.DepPostgres)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: b.podLabels(config.DepPostgres, componentDatabase)},
				Spec: b.podSpec(img.uid, img.gid, corev1.RestartPolicyAlways,
					[]corev1.Volume{scratch("run", resource.MustParse("16Mi")), scratch("tmp", tmpSize)}, c),
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			RevisionHistoryLimit: ptr(int32(2)),
		},
	}
}

// redis is a cache: no persistence, password required, memory capped below the
// container limit with LRU eviction so it evicts keys instead of being
// OOM-killed.
func (b *builder) redis(r *config.Redis) *appsv1.Deployment {
	img := b.dependencyImage(config.DepRedis, r.Version)
	res := b.dependencyResources(config.DepRedis)
	maxMemoryMi := (res.Limits.Memory().Value() >> 20) * 3 / 4

	c := container(config.DepRedis, img.ref(b.platform.ImageMirror), true)
	c.Args = []string{
		"redis-server",
		"--requirepass", "$(REDIS_PASSWORD)",
		"--save", "", "--appendonly", "no",
		"--maxmemory", fmt.Sprintf("%dmb", maxMemoryMi), "--maxmemory-policy", "allkeys-lru",
	}
	c.Env = []corev1.EnvVar{credential("REDIS_PASSWORD", keyRedisPassword)}
	c.Ports = []corev1.ContainerPort{{Name: config.DepRedis, ContainerPort: redisPort, Protocol: corev1.ProtocolTCP}}
	c.Resources = res
	c.ReadinessProbe = tcpProbe(config.DepRedis, 2)
	c.VolumeMounts = []corev1.VolumeMount{mount("data", "/data"), mount("tmp", "/tmp")}

	return b.dependencyDeployment(config.DepRedis, componentCache, img,
		[]corev1.Volume{scratch("data", resource.MustParse("64Mi")), scratch("tmp", tmpSize)}, c)
}

// rabbitMQ runs a single broker with a generated user. Queues are scratch
// state: reset purges them anyway (P2).
func (b *builder) rabbitMQ(r *config.RabbitMQ) *appsv1.Deployment {
	img := b.dependencyImage(config.DepRabbitMQ, r.Version)
	c := container(config.DepRabbitMQ, img.ref(b.platform.ImageMirror), true)
	c.Env = []corev1.EnvVar{
		{Name: "RABBITMQ_DEFAULT_USER", Value: rabbitMQUser},
		credential("RABBITMQ_DEFAULT_PASS", keyRabbitMQPassword),
		// The working directory is the read-only root; crash dumps go here.
		{Name: "ERL_CRASH_DUMP", Value: "/var/lib/rabbitmq/erl_crash.dump"},
		// Boot fast inside a quarter of a CPU: one Erlang scheduler (not one
		// per host core) without busy-waiting, and no plugins (previews only
		// need AMQP; the image enables Prometheus metrics). Measured boot at
		// 250m: 83s by default, 42s like this.
		{Name: "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS", Value: "+S 1:1 +sbwt none +sbwtdcpu none +sbwtdio none"},
		{Name: "RABBITMQ_ENABLED_PLUGINS_FILE", Value: "/var/lib/rabbitmq/enabled_plugins"},
	}
	c.Ports = []corev1.ContainerPort{{Name: config.DepRabbitMQ, ContainerPort: rabbitMQPort, Protocol: corev1.ProtocolTCP}}
	c.Resources = b.dependencyResources(config.DepRabbitMQ)
	// The AMQP listener opens last during boot, so a TCP check means ready.
	c.ReadinessProbe = tcpProbe(config.DepRabbitMQ, 10)
	c.VolumeMounts = []corev1.VolumeMount{mount("data", "/var/lib/rabbitmq"), mount("log", "/var/log/rabbitmq"), mount("tmp", "/tmp")}

	return b.dependencyDeployment(config.DepRabbitMQ, componentBroker, img, []corev1.Volume{
		scratch("data", resource.MustParse("512Mi")), scratch("log", resource.MustParse("64Mi")), scratch("tmp", tmpSize),
	}, c)
}

// dependencyDeployment recreates rather than rolls: two brokers or caches
// with separate in-memory state would split the environment.
func (b *builder) dependencyDeployment(name, component string, img catalogImage, volumes []corev1.Volume, c corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   typeDeployment,
		ObjectMeta: b.meta(name, StageDependencies, component),
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr(int32(1)),
			Selector:             &metav1.LabelSelector{MatchLabels: selector(name)},
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit: ptr(int32(2)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: b.podLabels(name, component)},
				Spec:       b.podSpec(img.uid, img.gid, corev1.RestartPolicyAlways, volumes, c),
			},
		},
	}
}
