package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
)

const document = `apiVersion: heimdall.dev/v1alpha1
kind: DesiredState
revision: "7"
environments:
  - name: acme-demo-pr7
    spec:
      tenant: acme
      repository: acme/demo
      pullRequest: 7
      commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      generation: 1
      environmentID: env-7
      owner: octocat
      urlSuffix: abcd
      expiresAt: "2026-10-04T12:00:00Z"
      config: {inline: "version: 1\n", sha256: "0000000000000000000000000000000000000000000000000000000000000000"}
`

func TestParseDocument(t *testing.T) {
	s, err := ParseDocument([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != "7" || len(s.Environments) != 1 || s.Environments[0].Spec.PullRequest != 7 {
		t.Fatalf("snapshot: %+v", s)
	}
	empty, err := ParseDocument([]byte("apiVersion: heimdall.dev/v1alpha1\nkind: DesiredState\nenvironments: []\n"))
	if err != nil || len(empty.Environments) != 0 {
		t.Fatalf("an explicit empty list is authoritative: %+v %v", empty, err)
	}
}

func TestParseDocumentRejects(t *testing.T) {
	tests := map[string]string{
		"empty":                "",
		"environments omitted": "apiVersion: heimdall.dev/v1alpha1\nkind: DesiredState\n",
		"wrong kind":           strings.Replace(document, "DesiredState", "Something", 1),
		"wrong version":        strings.Replace(document, "v1alpha1", "v9", 1),
		"unknown field":        strings.Replace(document, "revision:", "revisoin:", 1),
		"bad name":             strings.Replace(document, "acme-demo-pr7", "Acme_Demo", 1),
		"duplicate name":       document + strings.Join(strings.Split(document, "\n")[4:], "\n"),
		"too large":            document + "# " + strings.Repeat("x", MaxDocumentBytes),
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDocument([]byte(doc)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestConfigMapSource(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "desired", Namespace: "heimdall-system", ResourceVersion: "42"},
		Data: map[string]string{"desired-state.yaml": document}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	ctx := context.Background()

	s, err := (&ConfigMap{Reader: c, Namespace: "heimdall-system", Object: "desired", Key: "desired-state.yaml"}).Snapshot(ctx)
	if err != nil || len(s.Environments) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	for name, src := range map[string]*ConfigMap{
		"missing ConfigMap": {Reader: c, Namespace: "heimdall-system", Object: "absent", Key: "desired-state.yaml"},
		"missing key":       {Reader: c, Namespace: "heimdall-system", Object: "desired", Key: "other.yaml"},
	} {
		if _, err := src.Snapshot(ctx); err == nil {
			t.Errorf("%s must be unavailable, not empty", name)
		}
	}
}

func TestFileSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if _, err := (&File{Path: path}).Snapshot(context.Background()); err == nil {
		t.Fatal("a missing file must be unavailable, not empty")
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := (&File{Path: path}).Snapshot(context.Background()); err != nil || len(s.Environments) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestClusterSourceListsObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	pe := &v1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "pr7", Namespace: "heimdall-system"},
		Spec: v1alpha1.PreviewEnvironmentSpec{Repository: "acme/demo", PullRequest: 7}}
	other := pe.DeepCopy()
	other.Name, other.Namespace = "elsewhere", "default"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pe, other).Build()
	s, err := (&Cluster{Reader: c, Namespace: "heimdall-system"}).Snapshot(context.Background())
	if err != nil || len(s.Environments) != 1 || s.Environments[0].Name != "pr7" {
		t.Fatalf("%+v %v", s, err)
	}
}
