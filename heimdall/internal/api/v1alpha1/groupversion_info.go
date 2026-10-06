// Package v1alpha1 is the heimdall.dev/v1alpha1 API: the PreviewEnvironment
// custom resource the agent reconciles (docs/agent.md, ADR 0010). It depends
// only on k8s.io/apimachinery, so anything can import it cheaply.
//
// +kubebuilder:object:generate=true
// +groupName=heimdall.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version of this package.
	GroupVersion = schema.GroupVersion{Group: "heimdall.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this package with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &PreviewEnvironment{}, &PreviewEnvironmentList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
