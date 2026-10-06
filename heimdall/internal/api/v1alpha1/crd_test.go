//go:build !windows

// envtest does not build on Windows in controller-runtime v0.25; run on Linux
// or macOS (make envtest runs it in a container on Windows).

package v1alpha1_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
)

// These tests prove the CRD itself (schema and CEL rules) enforces the API's
// invariants in the real API server, with no webhook installed.

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("v1alpha1: envtest skipped; set KUBEBUILDER_ASSETS (make envtest)")
		os.Exit(m.Run())
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../../charts/heimdall-agent/crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("envtest:", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-system"}}); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func object(name string) *v1alpha1.PreviewEnvironment {
	doc := "version: 1\nservices: {}\n"
	sum := sha256.Sum256([]byte(doc))
	return &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "heimdall-system"},
		Spec: v1alpha1.PreviewEnvironmentSpec{
			Tenant: "acme", Repository: "acme/demo", PullRequest: 7, Commit: strings.Repeat("a", 40), Generation: 2,
			EnvironmentID: "env-7", Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now()),
			ResetNonce: 3,
			Config:     v1alpha1.ConfigSource{Inline: doc, SHA256: hex.EncodeToString(sum[:])},
			Images:     map[string]string{"app": "ghcr.io/acme/app@sha256:" + strings.Repeat("1", 64)},
		},
	}
}

func TestDefaults(t *testing.T) {
	if k8s == nil {
		t.Skip("requires envtest")
	}
	pe := object("defaults")
	pe.Spec.ResetNonce = 0
	if err := k8s.Create(context.Background(), pe); err != nil {
		t.Fatal(err)
	}
	if pe.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Errorf("desiredState default = %q", pe.Spec.DesiredState)
	}
}

func TestCreateValidation(t *testing.T) {
	if k8s == nil {
		t.Skip("requires envtest")
	}
	tests := map[string]func(*v1alpha1.PreviewEnvironment){
		"sleeping is not supported until P8": func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.DesiredState = "Sleeping" },
		"bad repository":                     func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Repository = "no-owner" },
		"generation zero":                    func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Generation = 0 },
		"short commit":                       func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Commit = "abc123" },
		"bad digest":                         func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Config.SHA256 = "XYZ" },
		"unsanitised data": func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.Data = &v1alpha1.DataSource{ConfigMap: "data", Key: "seed.sql", Approval: v1alpha1.DataApproval{
				SHA256: strings.Repeat("a", 64), ApprovedBy: "ops", Reason: "test", Sanitised: false}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pe := object("create-" + strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), "8", "eight"))
			mutate(pe)
			if err := k8s.Create(context.Background(), pe); !apierrors.IsInvalid(err) {
				t.Fatalf("want Invalid, got %v", err)
			}
		})
	}
}

func TestTransitionRules(t *testing.T) {
	if k8s == nil {
		t.Skip("requires envtest")
	}
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.PreviewEnvironment)
		allowed bool
		message string
	}{
		{"generation decrease", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Generation = 1 }, false, "must not decrease"},
		{"reset nonce decrease", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.ResetNonce = 2 }, false, "resetNonce must not decrease"},
		{"commit change without a new generation", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Commit = strings.Repeat("b", 40) }, false, "require a higher spec.generation"},
		{"image change without a new generation", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.Images["app"] = "ghcr.io/acme/app@sha256:" + strings.Repeat("2", 64)
		}, false, "require a higher spec.generation"},
		{"images removed without a new generation", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Images = nil }, false, "require a higher spec.generation"},
		{"repository is immutable", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.Repository = "acme/other" }, false, "repository is immutable"},
		{"url suffix is immutable", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.URLSuffix = "wxyz" }, false, "urlSuffix is immutable"},
		{"commit change with a new generation", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.Commit, pe.Spec.Generation = strings.Repeat("b", 40), 3
		}, true, ""},
		{"reset nonce increase", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.ResetNonce = 4 }, true, ""},
		{"expiry and desired state are not deployment inputs", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.ExpiresAt, pe.Spec.DesiredState = metav1.NewTime(time.Now().Add(time.Hour)), v1alpha1.DesiredDestroyed
		}, true, ""},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pe := object(fmt.Sprintf("transition-%d", i))
			ctx := context.Background()
			if err := k8s.Create(ctx, pe); err != nil {
				t.Fatal(err)
			}
			tc.mutate(pe)
			err := k8s.Update(ctx, pe)
			switch {
			case tc.allowed && err != nil:
				t.Fatalf("rejected: %v", err)
			case !tc.allowed && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.message)):
				t.Fatalf("want Invalid with %q, got %v", tc.message, err)
			}
		})
	}
}
