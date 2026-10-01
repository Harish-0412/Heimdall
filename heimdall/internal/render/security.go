package render

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Secure defaults rendered into every pod. heimdall.yaml cannot express any of
// these (ADR 0006), and every pod is built through podSpec and
// containerSecurity, so there is one place to audit. The namespace also
// enforces Pod Security "restricted" as a second line of defence.
//
//   - no Kubernetes API credentials: a dedicated service account, and token
//     automounting disabled on both the account and the pod
//   - non-root user, no privilege escalation, all capabilities dropped,
//     seccomp RuntimeDefault, read-only root filesystem with a sized /tmp
//   - no host namespaces, host paths or host ports; no service-link env vars
//   - no cloud credentials: nothing is mounted beyond what is listed here

// WorkloadServiceAccount runs every preview pod. It has no RBAC bindings and
// no token.
const WorkloadServiceAccount = "heimdall-workload"

// appUID runs application containers. Images whose default user is root (for
// example node or python) still run, as an unprivileged user.
const (
	appUID int64 = 10001
	appGID int64 = 10001
)

var tmpSize = resource.MustParse("256Mi")

func ptr[T any](v T) *T { return &v }

func podSecurity(uid, gid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:        ptr(true),
		RunAsUser:           ptr(uid),
		RunAsGroup:          ptr(gid),
		FSGroup:             ptr(gid),
		FSGroupChangePolicy: ptr(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurity(readOnlyRoot bool) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged:               ptr(false),
		AllowPrivilegeEscalation: ptr(false),
		RunAsNonRoot:             ptr(true),
		ReadOnlyRootFilesystem:   ptr(readOnlyRoot),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// podSpec wraps containers in the platform's pod-level defaults.
func (b *builder) podSpec(uid, gid int64, restart corev1.RestartPolicy, volumes []corev1.Volume, containers ...corev1.Container) corev1.PodSpec {
	spec := corev1.PodSpec{
		ServiceAccountName:           WorkloadServiceAccount,
		AutomountServiceAccountToken: ptr(false),
		EnableServiceLinks:           ptr(false),
		SecurityContext:              podSecurity(uid, gid),
		RestartPolicy:                restart,
		Volumes:                      volumes,
		Containers:                   containers,
		NodeSelector:                 maps.Clone(b.platform.NodeSelector),
		Tolerations:                  slices.Clone(b.platform.Tolerations),
	}
	if b.platform.RuntimeClassName != "" {
		spec.RuntimeClassName = ptr(b.platform.RuntimeClassName)
	}
	return spec
}

// container sets the fields every container shares.
func container(name, image string, readOnlyRoot bool) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent, // digests are immutable
		SecurityContext: containerSecurity(readOnlyRoot),
		// The last log lines become the termination message when the process
		// does not write one: the diagnostics engine (P4) reads it.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

// scratch is a size-limited writable directory, so a read-only root
// filesystem stays usable and a full disk evicts only this pod.
func scratch(name string, size resource.Quantity) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}}
}

func mount(name, path string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: path}
}

func readOnlyMount(name, path string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: true}
}
