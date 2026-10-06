// Package engine reconciles an explicit deployment specification. Kubernetes
// labels scope ownership; they never supply desired configuration.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/render"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const FieldManager = "heimdall-engine"
const journalName = "heimdall-operation"
const managedBy = "app.kubernetes.io/managed-by"

// Spec is supplied by a trusted caller, never reconstructed from labels.
// Reusing a generation with different desired content is rejected.
type Spec struct {
	Config       *config.Config
	Context      render.Context
	SeedApproval *SeedApproval
}

// SeedApproval is an operator attestation, bound to the exact reviewed bytes.
// Synthetic seeds are unsupported. Empty databases need no approval.
type SeedApproval struct {
	SHA256     string `json:"sha256"`
	ApprovedBy string `json:"approvedBy"`
	Reason     string `json:"reason"`
	Sanitised  bool   `json:"sanitised"`
}

type Error struct {
	Code, Message string
	Cause         error
}

func (e *Error) Error() string                      { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error                      { return e.Cause }
func failure(code, message string, err error) error { return &Error{code, message, err} }

type Event struct {
	Operation  string        `json:"operation"`
	Stage      string        `json:"stage"`
	State      string        `json:"state"`
	Generation int64         `json:"generation"`
	At         time.Time     `json:"at"`
	Duration   time.Duration `json:"durationNs,omitempty"`
	Code       string        `json:"code,omitempty"`
}
type Observer func(Event)
type Result struct {
	Namespace  string       `json:"namespace"`
	Generation int64        `json:"generation"`
	Phase      string       `json:"phase"`
	URLs       []render.URL `json:"urls,omitempty"`
	Events     []Event      `json:"events,omitempty"`
}

// Cluster is the infrastructure port. Kubernetes implements it with SSA,
// resourceVersion/UID preconditions and reconnecting watches.
type Cluster interface {
	Get(context.Context, Resource, string, string) (*unstructured.Unstructured, error)
	List(context.Context, Resource, string, string) ([]unstructured.Unstructured, error)
	Apply(context.Context, Resource, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Create(context.Context, Resource, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Update(context.Context, Resource, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Delete(context.Context, Resource, *unstructured.Unstructured) error
	Wait(context.Context, Resource, *unstructured.Unstructured, bool) error
}

type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

func res(group, version, plural, kind string, namespaced bool) Resource {
	return Resource{schema.GroupVersionResource{Group: group, Version: version, Resource: plural}, kind, namespaced}
}

var (
	namespaces   = res("", "v1", "namespaces", "Namespace", false)
	configmaps   = res("", "v1", "configmaps", "ConfigMap", true)
	secrets      = res("", "v1", "secrets", "Secret", true)
	pods         = res("", "v1", "pods", "Pod", true)
	claims       = res("", "v1", "persistentvolumeclaims", "PersistentVolumeClaim", true)
	deployments  = res("apps", "v1", "deployments", "Deployment", true)
	statefulsets = res("apps", "v1", "statefulsets", "StatefulSet", true)
	jobs         = res("batch", "v1", "jobs", "Job", true)
	inventory    = []Resource{jobs, deployments, statefulsets,
		res("gateway.networking.k8s.io", "v1", "httproutes", "HTTPRoute", true),
		res("", "v1", "services", "Service", true), configmaps, secrets,
		res("networking.k8s.io", "v1", "networkpolicies", "NetworkPolicy", true),
		res("", "v1", "resourcequotas", "ResourceQuota", true),
		res("", "v1", "limitranges", "LimitRange", true),
		res("", "v1", "serviceaccounts", "ServiceAccount", true),
	}
)

func resourceFor(o *unstructured.Unstructured) (Resource, error) {
	if o.GetKind() == namespaces.Kind {
		return namespaces, nil
	}
	for _, r := range inventory {
		if o.GetKind() == r.Kind && o.GroupVersionKind().Group == r.GVR.Group {
			return r, nil
		}
	}
	return Resource{}, fmt.Errorf("unsupported rendered kind %s", o.GetKind())
}
func hash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type Engine struct {
	cluster Cluster
	timeout time.Duration
	observe Observer
	// watchdog is how often a wait checks the workload's pods for a failure
	// that cannot recover (diagnose.Blocked).
	watchdog time.Duration
}

func New(c Cluster, timeout time.Duration, observer Observer) *Engine {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Engine{cluster: c, timeout: timeout, observe: observer, watchdog: 10 * time.Second}
}
