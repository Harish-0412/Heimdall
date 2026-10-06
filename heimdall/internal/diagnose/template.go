package diagnose

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// PodSpecMatchesTemplate compares a requested template with an observed pod,
// allowing API defaults and pod-only fields. Empty commands, arguments and
// literal environment values are intentional values, rather than wildcards:
// clearing one must not match a previous rollout revision that still has it.
func PodSpecMatchesTemplate(template, pod corev1.PodSpec) bool {
	return containersMatchTemplate(template.Containers, pod.Containers) &&
		containersMatchTemplate(template.InitContainers, pod.InitContainers) &&
		equality.Semantic.DeepDerivative(template, pod)
}

func containersMatchTemplate(template, pod []corev1.Container) bool {
	if len(template) > len(pod) {
		return false
	}
	for i, wanted := range template {
		actual := pod[i]
		if !slices.Equal(wanted.Command, actual.Command) || !slices.Equal(wanted.Args, actual.Args) || len(wanted.Env) != len(actual.Env) {
			return false
		}
		for j, env := range wanted.Env {
			observed := actual.Env[j]
			if env.Name != observed.Name || env.Value != observed.Value || (env.ValueFrom == nil) != (observed.ValueFrom == nil) {
				return false
			}
			// The derivative comparison still allows defaults within sources,
			// such as fieldRef.apiVersion and optional=false on Secret refs.
		}
	}
	return true
}
