// Package source supplies the agent's desired state (ADR 0005, ADR 0010).
// A Source returns the complete set of environments this cluster should run,
// obtained authoritatively, or an error. Callers must treat an error as
// "unknown", never as "empty": nothing may be deleted on the strength of a
// failed read. This is what makes the sweeper fail safe.
//
// P3 ships three sources: the cluster's own PreviewEnvironment objects
// (kubectl apply), a document in a ConfigMap, and a document on disk. P5 adds
// the control plane's API (an outbound pull) behind the same interface.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/strictyaml"
)

// Environment is one desired environment: the PreviewEnvironment's name and
// spec.
type Environment struct {
	Name string                          `json:"name"`
	Spec v1alpha1.PreviewEnvironmentSpec `json:"spec"`
}

// Snapshot is the complete desired state at one moment.
type Snapshot struct {
	// Revision identifies the snapshot, for logs.
	Revision     string
	Environments []Environment
}

// Source supplies desired state.
type Source interface {
	// Name identifies the source; synced objects are labelled with it.
	Name() string
	// Snapshot returns the complete desired state, or an error when it could
	// not be obtained authoritatively.
	Snapshot(ctx context.Context) (*Snapshot, error)
	// Managed reports whether the agent writes PreviewEnvironment objects
	// from this source (false when the objects themselves are the source).
	Managed() bool
}

// LabelSource marks PreviewEnvironment objects written from a source.
const LabelSource = "heimdall.dev/source"

// MaxDocumentBytes bounds a desired-state document.
const MaxDocumentBytes = 4 << 20

// Document is the desired-state file format.
//
//	apiVersion: heimdall.dev/v1alpha1
//	kind: DesiredState
//	revision: "42"
//	environments:
//	  - name: acme-shopflow-pr184
//	    spec: {...PreviewEnvironment spec...}
type Document struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Revision   string `json:"revision,omitempty"`
	// Environments must be present; write [] for "none". A document that
	// omits it is rejected rather than read as "delete everything".
	Environments *[]Environment `json:"environments"`
}

// ParseDocument decodes a desired-state document strictly.
func ParseDocument(data []byte) (*Snapshot, error) {
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("desired state is larger than %d bytes", MaxDocumentBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("desired state document is empty")
	}
	var d Document
	if err := strictyaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("desired state: %w", err)
	}
	if d.APIVersion != v1alpha1.GroupVersion.String() || d.Kind != "DesiredState" {
		return nil, fmt.Errorf("desired state: want apiVersion %s and kind DesiredState", v1alpha1.GroupVersion)
	}
	if d.Environments == nil {
		return nil, errors.New("desired state: environments is required (use [] for none)")
	}
	seen := map[string]bool{}
	for i, e := range *d.Environments {
		if errs := validation.IsDNS1123Subdomain(e.Name); len(errs) > 0 {
			return nil, fmt.Errorf("desired state: environments[%d].name %q: %s", i, e.Name, strings.Join(errs, "; "))
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("desired state: environment %q appears twice", e.Name)
		}
		seen[e.Name] = true
	}
	return &Snapshot{Revision: d.Revision, Environments: *d.Environments}, nil
}

// Cluster treats the PreviewEnvironment objects in a namespace as the desired
// state (operators kubectl-apply them). It reads through the API server, not
// a cache, so a snapshot is never older than the request.
type Cluster struct {
	Reader    client.Reader
	Namespace string
}

func (c *Cluster) Name() string  { return "cluster" }
func (c *Cluster) Managed() bool { return false }

func (c *Cluster) Snapshot(ctx context.Context) (*Snapshot, error) {
	var list v1alpha1.PreviewEnvironmentList
	if err := c.Reader.List(ctx, &list, client.InNamespace(c.Namespace)); err != nil {
		return nil, fmt.Errorf("list PreviewEnvironments: %w", err)
	}
	s := &Snapshot{Revision: list.ResourceVersion}
	for _, pe := range list.Items {
		s.Environments = append(s.Environments, Environment{Name: pe.Name, Spec: pe.Spec})
	}
	return s, nil
}

// ConfigMap reads a document from a ConfigMap key. A missing ConfigMap or
// key is "unavailable", not "empty".
type ConfigMap struct {
	Reader    client.Reader
	Namespace string
	Object    string // the ConfigMap's name
	Key       string
}

func (c *ConfigMap) Name() string  { return "configmap" }
func (c *ConfigMap) Managed() bool { return true }

func (c *ConfigMap) Snapshot(ctx context.Context) (*Snapshot, error) {
	var cm corev1.ConfigMap
	if err := c.Reader.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Object}, &cm); err != nil {
		return nil, fmt.Errorf("read ConfigMap %s/%s: %w", c.Namespace, c.Object, err)
	}
	data, ok := cm.Data[c.Key]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s has no key %q", c.Namespace, c.Object, c.Key)
	}
	s, err := ParseDocument([]byte(data))
	if err == nil && s.Revision == "" {
		s.Revision = cm.ResourceVersion
	}
	return s, err
}

// File reads a document from disk (running the agent outside a cluster).
type File struct{ Path string }

func (f *File) Name() string  { return "file" }
func (f *File) Managed() bool { return true }

func (f *File) Snapshot(context.Context) (*Snapshot, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return ParseDocument(data)
}
