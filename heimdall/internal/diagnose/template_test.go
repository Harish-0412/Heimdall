package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodTemplateClearedValuesAreNotWildcards(t *testing.T) {
	base := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry/api@sha256:current"}}}
	for _, tc := range []struct {
		name string
		old  func(*corev1.Container)
	}{
		{"removed command", func(c *corev1.Container) { c.Command = []string{"sh", "-c", "old-command"} }},
		{"removed arguments", func(c *corev1.Container) { c.Args = []string{"--old-mode"} }},
		{"empty literal env", func(c *corev1.Container) { c.Env[0].Value = "previous-value" }},
		{"removed env source", func(c *corev1.Container) {
			c.Env[0].ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "old-secret"}, Key: "MODE"}}
		}},
		{"removed env variable", func(c *corev1.Container) { c.Env = append(c.Env, corev1.EnvVar{Name: "OLD", Value: "value"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := base.DeepCopy()
			template.Containers[0].Env = []corev1.EnvVar{{Name: "MODE", Value: ""}}
			old := template.DeepCopy()
			tc.old(&old.Containers[0])
			if PodSpecMatchesTemplate(*template, *old) {
				t.Error("cleared field matched the previous revision")
			}
			current := template.DeepCopy()
			current.Containers[0].Command = []string{}
			current.Containers[0].Args = []string{}
			current.NodeName = "node-a"
			current.DNSPolicy = corev1.DNSClusterFirst
			current.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
			if !PodSpecMatchesTemplate(*template, *current) {
				t.Error("API defaults or nil/empty slices excluded the current revision")
			}
		})
	}
}

func TestPodTemplateAllowsEnvironmentSourceDefaults(t *testing.T) {
	falseValue := false
	template := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{
		{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"}, Key: "KEY"}}},
	}}}}
	actual := template.DeepCopy()
	actual.Containers[0].Env[0].ValueFrom.FieldRef.APIVersion = "v1"
	actual.Containers[0].Env[1].ValueFrom.SecretKeyRef.Optional = &falseValue
	if !PodSpecMatchesTemplate(template, *actual) {
		t.Error("defaulted environment source excluded the current revision")
	}
	actual.Containers[0].Env[1].ValueFrom.SecretKeyRef.Name = "previous-secret"
	if PodSpecMatchesTemplate(template, *actual) {
		t.Error("changed secret reference matched the current revision")
	}
}
