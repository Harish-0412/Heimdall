package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/render"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

type Kubernetes struct{ Client dynamic.Interface }

func (k *Kubernetes) resource(r Resource, ns string) dynamic.ResourceInterface {
	if r.Namespaced {
		return k.Client.Resource(r.GVR).Namespace(ns)
	}
	return k.Client.Resource(r.GVR)
}
func (k *Kubernetes) Get(ctx context.Context, r Resource, ns, name string) (*unstructured.Unstructured, error) {
	return k.resource(r, ns).Get(ctx, name, metav1.GetOptions{})
}
func (k *Kubernetes) List(ctx context.Context, r Resource, ns, selector string) ([]unstructured.Unstructured, error) {
	list, err := k.resource(r, ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
func (k *Kubernetes) Apply(ctx context.Context, r Resource, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var result *unstructured.Unstructured
	err := retry.OnError(wait.Backoff{Steps: 5, Duration: 20 * time.Millisecond, Factor: 2}, func(err error) bool {
		return apierrors.IsConflict(err) && !fieldConflict(err)
	}, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := k.Get(ctx, r, o.GetNamespace(), o.GetName())
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		request := o.DeepCopy()
		if err == nil {
			if o.GetLabels()[render.LabelEnv] != "" {
				for _, key := range []string{render.LabelTenant, render.LabelRepo, render.LabelPR, render.LabelEnv, managedBy} {
					if current.GetLabels()[key] != o.GetLabels()[key] {
						return failure("engine.ownership", "object ownership changed before apply", nil)
					}
				}
			}
			request.SetResourceVersion(current.GetResourceVersion())
		}
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		result, err = k.resource(r, o.GetNamespace()).Patch(ctx, o.GetName(), types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: FieldManager})
		if selfConflict(err) {
			// Kubernetes distinguishes Apply from Update even for the same
			// manager (namespace creation and quiescing replicas). Transfer only
			// our own Update fields. Pin the exact RV used in the failed apply,
			// so a concurrent manager cannot be overwritten by this retry.
			force := true
			result, err = k.resource(r, o.GetNamespace()).Patch(ctx, o.GetName(), types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
		}
		return err
	})
	return result, err
}

func fieldConflict(err error) bool {
	var status *apierrors.StatusError
	if !errors.As(err, &status) || status.ErrStatus.Details == nil {
		return false
	}
	for _, cause := range status.ErrStatus.Details.Causes {
		if cause.Type == metav1.CauseTypeFieldManagerConflict {
			return true
		}
	}
	return false
}
func selfConflict(err error) bool {
	var status *apierrors.StatusError
	if !errors.As(err, &status) || status.ErrStatus.Details == nil || len(status.ErrStatus.Details.Causes) == 0 {
		return false
	}
	for _, cause := range status.ErrStatus.Details.Causes {
		if cause.Type != metav1.CauseTypeFieldManagerConflict || !strings.HasPrefix(cause.Message, `conflict with "`+FieldManager+`"`) {
			return false
		}
	}
	return true
}
func (k *Kubernetes) Create(ctx context.Context, r Resource, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return k.resource(r, o.GetNamespace()).Create(ctx, o, metav1.CreateOptions{FieldManager: FieldManager})
}
func (k *Kubernetes) Update(ctx context.Context, r Resource, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return k.resource(r, o.GetNamespace()).Update(ctx, o, metav1.UpdateOptions{FieldManager: FieldManager})
}
func (k *Kubernetes) Delete(ctx context.Context, r Resource, o *unstructured.Unstructured) error {
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		current, err := k.Get(ctx, r, o.GetNamespace(), o.GetName())
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.GetUID() != o.GetUID() {
			return nil
		}
		for _, key := range []string{render.LabelTenant, render.LabelRepo, render.LabelPR, render.LabelEnv, render.LabelGeneration, managedBy} {
			if current.GetLabels()[key] != o.GetLabels()[key] {
				return failure("engine.delete_fenced", "object ownership or generation changed before deletion", nil)
			}
		}
		uid, rv := current.GetUID(), current.GetResourceVersion()
		propagation := metav1.DeletePropagationForeground
		err = k.resource(r, o.GetNamespace()).Delete(ctx, o.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &propagation})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

// Wait re-lists after expired watches. A condition is evaluated on the initial
// list too, so completion/deletion between apply and watch cannot be missed.
func (k *Kubernetes) Wait(ctx context.Context, r Resource, o *unstructured.Unstructured, gone bool) error {
	c := k.resource(r, o.GetNamespace())
	selector := fields.OneTermEqualSelector("metadata.name", o.GetName()).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(cctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = selector
			return c.List(cctx, opts)
		},
		WatchFuncWithContext: func(cctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = selector
			return c.Watch(cctx, opts)
		},
	}
	check := func(current *unstructured.Unstructured) (bool, error) {
		if current.GetUID() != o.GetUID() {
			if gone {
				return true, nil
			}
			return false, failure("engine.replaced", "object was replaced while awaiting readiness", nil)
		}
		if gone {
			return false, nil
		}
		return Ready(current)
	}
	_, err := watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, func(store cache.Store) (bool, error) {
		objects := store.List()
		if len(objects) == 0 {
			if gone {
				return true, nil
			}
			return false, failure("engine.disappeared", "object disappeared while awaiting readiness", nil)
		}
		return check(objects[0].(*unstructured.Unstructured))
	}, func(ev watch.Event) (bool, error) {
		if ev.Type == watch.Error {
			return false, apierrors.FromObject(ev.Object)
		}
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, fmt.Errorf("unexpected watch object")
		}
		if ev.Type == watch.Deleted {
			if gone {
				return true, nil
			}
			return false, failure("engine.disappeared", "object was deleted while awaiting readiness", nil)
		}
		return check(u)
	})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func number(o *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedInt64(o.Object, fields...)
	return v
}
func str(o *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(o.Object, fields...)
	return v
}
func conditions(o *unstructured.Unstructured) []any {
	v, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	return v
}

// Ready checks observedGeneration and the complete desired replica count;
// Available alone can refer to the previous rollout.
func Ready(o *unstructured.Unstructured) (bool, error) {
	if o.GetDeletionTimestamp() != nil {
		return false, nil
	}
	switch o.GetKind() {
	case "Job":
		for _, raw := range conditions(o) {
			c, ok := raw.(map[string]any)
			if !ok || c["status"] != "True" {
				continue
			}
			if c["type"] == "Failed" || c["type"] == "FailureTarget" {
				return false, failure("engine.job_failed", "Job "+o.GetName()+" failed; inspect its logs", nil)
			}
			if c["type"] == "Complete" {
				return true, nil
			}
		}
		return false, nil
	case "Deployment", "StatefulSet":
		if number(o, "status", "observedGeneration") < o.GetGeneration() {
			return false, nil
		}
		desired := number(o, "spec", "replicas")
		// A failed rollout can still be drained successfully. Its historical
		// Progressing condition must not prevent scaling it to zero for recovery.
		if o.GetKind() == "Deployment" && desired > 0 {
			for _, raw := range conditions(o) {
				c, ok := raw.(map[string]any)
				if ok && c["type"] == "Progressing" && c["status"] == "False" && c["reason"] == "ProgressDeadlineExceeded" {
					return false, failure("engine.rollout_failed", "Deployment "+o.GetName()+" exceeded its rollout deadline", nil)
				}
			}
		}
		ready := number(o, "status", "replicas") == desired && number(o, "status", "updatedReplicas") == desired &&
			number(o, "status", "readyReplicas") == desired
		// Available honours minReadySeconds: a pod must also have stayed
		// ready that long (workers, which have no health check).
		if o.GetKind() == "Deployment" {
			ready = ready && number(o, "status", "availableReplicas") == desired
		}
		return ready, nil
	default:
		return true, nil
	}
}
