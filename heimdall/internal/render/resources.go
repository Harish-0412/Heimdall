package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// requirements converts resolved config resources (limits and requests both
// set, as Load guarantees) into Kubernetes form.
func requirements(r config.Resources) (corev1.ResourceRequirements, error) {
	var errs []error
	q := func(s string) resource.Quantity {
		v, err := resource.ParseQuantity(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("quantity %q: %w", s, err))
		}
		return v
	}
	out := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceCPU: q(r.CPU), corev1.ResourceMemory: q(r.Memory)},
		Requests: corev1.ResourceList{corev1.ResourceCPU: q(r.Requests.CPU), corev1.ResourceMemory: q(r.Requests.Memory)},
	}
	if len(errs) > 0 {
		return corev1.ResourceRequirements{}, fmt.Errorf("unresolved resources %+v: %v", r, errs)
	}
	return out, nil
}

// fixed returns requirements for a container Heimdall defines itself, with
// requests derived exactly as for workloads.
func (b *builder) fixed(cpu, memory string) corev1.ResourceRequirements {
	r, err := requirements(config.ResolveResources(config.Resources{CPU: cpu, Memory: memory}, config.SizeSmall, b.ctx.Policy))
	if err != nil {
		panic(err) // constants in this package; a test covers every call site
	}
	return r
}

func milliQuantity(m int) resource.Quantity {
	return *resource.NewMilliQuantity(int64(m), resource.DecimalSI)
}

func mebiQuantity(mi int) resource.Quantity {
	return *resource.NewQuantity(int64(mi)<<20, resource.BinarySI)
}
