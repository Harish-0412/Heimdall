package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	typedauth "k8s.io/client-go/kubernetes/typed/authorization/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// Break-glass requires actual cluster-level authorisation, not a CLI boolean.
// It preserves a redacted on-disk audit bundle before touching finalizers.
func forceCleanup(ctx context.Context, cfg *rest.Config, e *engine.Engine, spec engine.Spec, reason, path string, timeout time.Duration) error {
	if strings.TrimSpace(reason) == "" || path == "" {
		return fmt.Errorf("force-cleanup requires --reason and --evidence")
	}
	ns, scope, err := e.Scope(spec)
	if err != nil {
		return err
	}
	auth, err := typedauth.NewForConfig(cfg)
	if err != nil {
		return err
	}
	for _, verb := range []string{"update", "delete"} {
		resource, subresource := "namespaces", ""
		if verb == "update" {
			subresource = "finalize"
		}
		review, err := auth.SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Resource: resource, Subresource: subresource, Name: ns}}}, metav1.CreateOptions{})
		if err != nil || !review.Status.Allowed {
			return fmt.Errorf("engine.admin_required: cluster permission to %s namespaces/%s is required", verb, subresource)
		}
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	core, err := typedcore.NewForConfig(cfg)
	if err != nil {
		return err
	}
	namespace, err := core.Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for k, v := range scope {
		if namespace.Labels[k] != v {
			return fmt.Errorf("engine.ownership: namespace identity mismatch")
		}
	}
	if namespace.Labels[render.LabelPreview] != "true" || namespace.DeletionTimestamp == nil {
		return fmt.Errorf("force-cleanup is only for an owned preview already stuck terminating; run down first")
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	lists, err := disco.ServerPreferredNamespacedResources()
	if err != nil {
		return fmt.Errorf("cannot safely enumerate namespace resources: %w", err)
	}
	type entry struct {
		r schema.GroupVersionResource
		o unstructured.Unstructured
	}
	var objects []entry
	var evidence []any
	values, err := secretValues(ctx, core, ns)
	if err != nil {
		return err
	}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return err
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "delete") {
				continue
			}
			gvr := gv.WithResource(r.Name)
			items, err := client.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("cannot preserve resource evidence for %s", r.Name)
			}
			for _, o := range items.Items {
				snapshot := o.DeepCopy()
				delete(snapshot.Object, "data")
				delete(snapshot.Object, "binaryData")
				delete(snapshot.Object, "stringData")
				// Pod specs can contain application environment literals. Preserve runtime
				// status and metadata; no Secret, seed contents or container command bodies.
				delete(snapshot.Object, "spec")
				delete(snapshot.Object, "metadata")
				snapshot.Object["metadata"] = map[string]any{"name": o.GetName(), "namespace": ns, "uid": string(o.GetUID()), "resourceVersion": o.GetResourceVersion(), "finalizers": o.GetFinalizers(), "labels": o.GetLabels()}
				evidence = append(evidence, snapshot.Object)
				objects = append(objects, entry{gvr, o})
			}
		}
	}
	bundle := map[string]any{"operation": "force-cleanup", "reason": reason, "at": time.Now().UTC(), "namespace": ns, "namespaceUID": namespace.UID, "generation": spec.Context.Generation, "eventsAndObjects": evidence}
	b, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	var document any
	if err = json.Unmarshal(b, &document); err != nil {
		return err
	}
	redactDocument(document, values)
	b, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	work, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, entry := range objects {
		o := entry.o.DeepCopy()
		if len(o.GetFinalizers()) == 0 {
			continue
		}
		for k, v := range scope {
			if o.GetLabels()[k] != v {
				return fmt.Errorf("engine.foreign_finalizer: refusing to strip finalizers from %s/%s; evidence saved", entry.r.Resource, o.GetName())
			}
		}
		o.SetFinalizers(nil)
		_, err = client.Resource(entry.r).Namespace(ns).Update(work, o, metav1.UpdateOptions{FieldManager: engine.FieldManager})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Namespace finalization may orphan resources. Only finalize once a fresh,
	// complete discovery-backed inventory proves the namespace is empty.
	for _, list := range lists {
		gv, _ := schema.ParseGroupVersion(list.GroupVersion)
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "delete") {
				continue
			}
			items, err := client.Resource(gv.WithResource(r.Name)).Namespace(ns).List(work, metav1.ListOptions{})
			if err != nil {
				return err
			}
			if len(items.Items) > 0 {
				return fmt.Errorf("engine.cleanup_pending: finalizers released; namespace controller still has %s to remove; retry down", r.Name)
			}
		}
	}
	namespace, err = core.Namespaces().Get(work, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	namespace.Spec.Finalizers = nil
	_, err = core.Namespaces().Finalize(work, namespace, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	for {
		_, err = core.Namespaces().Get(work, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-work.Done():
			return fmt.Errorf("engine.destroy_stuck: namespace still exists")
		case <-time.After(time.Second):
		}
	}
}

// Redact JSON string values before serialization so quoted secrets cannot
// corrupt the evidence document or evade exact-value redaction via escaping.
func redactDocument(value any, secrets []string) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if text, ok := item.(string); ok {
				v[key] = engine.Redact(text, secrets)
			} else {
				redactDocument(item, secrets)
			}
		}
	case []any:
		for i, item := range v {
			if text, ok := item.(string); ok {
				v[i] = engine.Redact(text, secrets)
			} else {
				redactDocument(item, secrets)
			}
		}
	}
}
