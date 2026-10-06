package render

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// CleanupPlan carries only immutable ownership and naming. Cleanup must remain
// possible after the config, policy, registry or approved data is unavailable.
// It authorizes no deployment and produces only a namespace ownership record.
func CleanupPlan(c Context) (*Plan, error) {
	if !tenantRE.MatchString(c.Tenant) || !repoRE.MatchString(c.Repo) || strings.HasSuffix(c.Repo, "/.") || strings.HasSuffix(c.Repo, "/..") || c.PR < 1 || c.PR > maxPR || c.Generation < 1 || len(validation.IsDNS1123Label(c.EnvironmentID)) > 0 || !suffixRE.MatchString(c.URLSuffix) {
		return nil, fmt.Errorf("invalid immutable cleanup identity")
	}
	b := &builder{ctx: c}
	labels := b.envLabels()
	labels[LabelPreview] = "true"
	ns := &corev1.Namespace{TypeMeta: typeNamespace, ObjectMeta: metav1.ObjectMeta{Name: NamespaceFor(c.Repo, c.PR, c.URLSuffix), Labels: labels}}
	return &Plan{Namespace: ns.Name, Stages: []Stage{{Name: StageGuardrails, Steps: []Step{{Name: "ownership", Objects: []Object{ns}}}}}}, nil
}
