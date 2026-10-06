package diagnose

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/heimdall-dev/heimdall/internal/render"
)

// routeRule explains a preview URL the Gateway does not serve: the route was
// not accepted by its parent Gateway, or its backend did not resolve. Every
// workload can be healthy while the URL is dead, so nothing else would
// explain it. A route without status says nothing (no Gateway controller has
// processed it, as on clusters without one) and yields no finding.
func routeRule(s *Snapshot, _ *index) []Diagnosis {
	var out []Diagnosis
	for _, r := range s.Routes {
		if r.DeletionTimestamp != nil || !s.current(r.Labels) {
			continue
		}
		url := r.Annotations[render.AnnotationURL]
		if url == "" && len(r.Spec.Hostnames) > 0 {
			url = string(r.Spec.Hostnames[0])
		}
		w := workloadOf(r.Labels)
		for _, p := range r.Status.Parents {
			gateway := parentName(r.Namespace, p.ParentRef)
			if len(r.Spec.ParentRefs) > 0 {
				currentParent := false
				for _, parent := range r.Spec.ParentRefs {
					// A parent without sectionName permits every listener;
					// controllers may report one parent status per listener.
					statusParent := p.ParentRef
					if parent.SectionName == nil {
						statusParent.SectionName = nil
					}
					currentParent = currentParent || parentName(r.Namespace, parent) == parentName(r.Namespace, statusParent)
				}
				if !currentParent {
					continue
				}
			}
			for _, c := range p.Conditions {
				if c.Status != metav1.ConditionFalse || (c.ObservedGeneration != 0 && c.ObservedGeneration != r.Generation) {
					continue
				}
				d := Diagnosis{Code: NoEndpoints, Subject: "httproute/" + r.Name, Workload: r.Name, Stage: w.stage,
					component: w.component, Evidence: []string{fmt.Sprintf("%s %s=False %s: %s", gateway, c.Type, c.Reason, c.Message)}}
				switch gatewayv1.RouteConditionType(c.Type) {
				case gatewayv1.RouteConditionAccepted:
					d.Summary = fmt.Sprintf("%s is not served: Gateway %s rejected route %s (%s)", url, gateway, r.Name, c.Reason)
					d.Suggestion = acceptedSuggestion(gatewayv1.RouteConditionReason(c.Reason), url)
				case gatewayv1.RouteConditionResolvedRefs:
					d.Summary = fmt.Sprintf("%s is not served: route %s points at a backend Gateway %s cannot use (%s)", url, r.Name, gateway, c.Reason)
					d.Suggestion = "The route's backend Service is missing or not allowed. Heimdall creates the Service with " +
						"the route: run `heimdall up` again, and check the Service exists."
					if gatewayv1.RouteConditionReason(c.Reason) == gatewayv1.RouteReasonRefNotPermitted {
						d.Suggestion = "The Gateway may not use a backend in another namespace without a ReferenceGrant; " +
							"preview routes only point at their own namespace, so the route was changed by hand."
					}
				default:
					continue
				}
				out = append(out, d)
			}
		}
	}
	return out
}

func acceptedSuggestion(reason gatewayv1.RouteConditionReason, url string) string {
	switch reason {
	case gatewayv1.RouteReasonNoMatchingListenerHostname:
		return "The preview's hostname (" + url + ") is outside every hostname the Gateway listens on. The platform's " +
			"baseDomain must be the zone of the listener's wildcard hostname (for example *.preview.example.com): a " +
			"platform administrator's fix, in the agent's values or the CLI's --base-domain."
	case gatewayv1.RouteReasonNotAllowedByListeners:
		return "The Gateway's listener does not admit routes from preview namespaces. Its allowedRoutes must select " +
			"namespaces labelled heimdall.dev/preview=true (ADR 0008): a platform administrator's fix."
	case gatewayv1.RouteReasonNoMatchingParent:
		return "The route names a Gateway listener that does not exist. Check platform.gateway (namespace, name, " +
			"sectionName) against the cluster's Gateway."
	}
	return "The Gateway rejected the route; its condition (evidence) says why."
}

func parentName(routeNamespace string, p gatewayv1.ParentReference) string {
	ns := routeNamespace
	if p.Namespace != nil {
		ns = string(*p.Namespace)
	}
	name := ns + "/" + string(p.Name)
	if p.SectionName != nil {
		name += " (listener " + string(*p.SectionName) + ")"
	}
	return name
}
