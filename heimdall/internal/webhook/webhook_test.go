package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controller"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const image = "ghcr.io/acme/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func environment(doc string) *v1alpha1.PreviewEnvironment {
	sum := sha256.Sum256([]byte(doc))
	return &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "pr7", Namespace: "heimdall-system"},
		Spec: v1alpha1.PreviewEnvironmentSpec{
			Tenant: "acme", Repository: "acme/demo", PullRequest: 7, Commit: strings.Repeat("a", 40), Generation: 1,
			EnvironmentID: "env-7", Owner: "octocat", URLSuffix: "abcd", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
			Config: v1alpha1.ConfigSource{Inline: doc, SHA256: hex.EncodeToString(sum[:])},
		},
	}
}

func validator(p config.Policy) *Validator {
	return &Validator{Specs: controller.SpecBuilder{Policy: p, Platform: render.Platform{BaseDomain: "preview.example.com"}}}
}

const validDoc = "version: 1\nservices:\n  app: {image: " + image + ", port: 80, public: true, health: {path: /h}}\n"

func TestValidatorAdmitsAValidEnvironment(t *testing.T) {
	warnings, err := validator(config.DefaultPolicy()).ValidateCreate(context.Background(), environment(validDoc))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings %v, err %v", warnings, err)
	}
}

func TestValidatorReturnsConfigWarnings(t *testing.T) {
	doc := "version: 1\nservices:\n  app: {image: " + image + ", port: 80, public: true}\n" // no health check
	warnings, err := validator(config.DefaultPolicy()).ValidateCreate(context.Background(), environment(doc))
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "health.missing") {
		t.Fatalf("warnings %v, err %v", warnings, err)
	}
}

func TestValidatorRejects(t *testing.T) {
	strict := config.DefaultPolicy()
	strict.MaxVisibility = config.VisibilityPrivate
	tests := []struct {
		name   string
		policy config.Policy
		pe     *v1alpha1.PreviewEnvironment
		want   string
	}{
		{"invalid config", config.DefaultPolicy(), environment("version: 1\nservices:\n  app: {image: " + image + ", port: 70000}\n"), "port.invalid"},
		{"tenant policy", strict, environment(validDoc + "preview: {visibility: public}\n"), "policy.visibility_denied"},
		{"digest mismatch", config.DefaultPolicy(), func() *v1alpha1.PreviewEnvironment {
			pe := environment(validDoc)
			pe.Spec.Config.SHA256 = strings.Repeat("0", 64)
			return pe
		}(), controller.CodeConfigDigest},
		{"unpinned image", config.DefaultPolicy(), environment("version: 1\nservices:\n  app: {image: nginx:1.27, port: 80, public: true, health: {path: /h}}\n"), "render.image.missing"},
		{"import without approval", config.DefaultPolicy(), environment(validDoc + "dependencies: {postgres: {seed: db/seed.sql}}\n"), controller.CodeDataMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validator(tc.policy).ValidateCreate(context.Background(), tc.pe)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an Invalid error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidatorAlwaysAdmitsReleaseAndMetadataUpdates(t *testing.T) {
	v := validator(config.DefaultPolicy())
	old := environment("version: 1\nservices: {}\n") // invalid, e.g. accepted under a laxer policy
	updated := old.DeepCopy()
	updated.Finalizers = nil
	if _, err := v.ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("metadata-only update rejected: %v", err)
	}
	deleting := old.DeepCopy()
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Spec.Commit = strings.Repeat("b", 40)
	if _, err := v.ValidateUpdate(context.Background(), old, deleting); err != nil {
		t.Fatalf("update of an object being deleted rejected: %v", err)
	}
}

// --- certificates ---

func certFixture(t *testing.T) (*Certificates, client.Client, *clocktesting.FakeClock) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-agent"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: "previewenvironments.heimdall.dev"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vwc).Build()
	clk := clocktesting.NewFakeClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	certs := &Certificates{Client: c, Reader: c, Namespace: "heimdall-system", SecretName: "heimdall-agent-webhook-tls",
		WebhookConfig: "heimdall-agent", DNSNames: []string{"heimdall-agent-webhook.heimdall-system.svc"}, Clock: clk, Log: logr.Discard()}
	return certs, c, clk
}

func secretOf(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "heimdall-system", Name: "heimdall-agent-webhook-tls"}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCertificatesIssueServeAndInject(t *testing.T) {
	certs, c, _ := certFixture(t)
	if _, err := certs.GetCertificate(nil); err == nil || certs.Ready(nil) == nil {
		t.Fatal("must not serve or be ready before issuing")
	}
	if err := certs.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := secretOf(t, c)
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "heimdall-agent"}, vwc); err != nil {
		t.Fatal(err)
	}
	if string(vwc.Webhooks[0].ClientConfig.CABundle) != string(s.Data[keyCACert]) {
		t.Fatal("CA bundle not injected")
	}
	served, err := certs.GetCertificate(nil)
	if err != nil || certs.Ready(nil) != nil {
		t.Fatalf("not serving: %v", err)
	}
	// The served certificate verifies against the injected CA for the
	// service name the API server dials.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(vwc.Webhooks[0].ClientConfig.CABundle)
	leaf, _ := x509.ParseCertificate(served.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "heimdall-agent-webhook.heimdall-system.svc",
		CurrentTime: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("served certificate does not verify: %v", err)
	}
}

func TestCertificatesAreStableUntilRenewalIsDue(t *testing.T) {
	certs, c, clk := certFixture(t)
	ctx := context.Background()
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	first := secretOf(t, c)
	clk.Step(300 * 24 * time.Hour)
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if string(secretOf(t, c).Data[keyCert]) != string(first.Data[keyCert]) {
		t.Fatal("certificate reissued before renewal was due")
	}
	clk.Step(40 * 24 * time.Hour) // within 30 days of expiry
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	renewed := secretOf(t, c)
	if string(renewed.Data[keyCert]) == string(first.Data[keyCert]) {
		t.Fatal("expiring certificate was not renewed")
	}
	if string(renewed.Data[keyCACert]) != string(first.Data[keyCACert]) {
		t.Fatal("renewal replaced a CA that was still valid for years")
	}
}

func TestCertificatesReplaceCorruptOrMismatchedSecrets(t *testing.T) {
	certs, c, _ := certFixture(t)
	ctx := context.Background()
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	s := secretOf(t, c)
	s.Data[keyCAKey] = []byte("garbage")
	if err := c.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(secretOf(t, c).Data); err != nil {
		t.Fatalf("corrupt secret not repaired: %v", err)
	}
	certs.DNSNames = []string{"renamed.heimdall-system.svc"}
	before := secretOf(t, c).Data[keyCert]
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if string(secretOf(t, c).Data[keyCert]) == string(before) {
		t.Fatal("a service rename did not reissue the certificate")
	}
	pair, _ := tls.X509KeyPair(secretOf(t, c).Data[keyCert], secretOf(t, c).Data[keyKey])
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if leaf.DNSNames[0] != "renamed.heimdall-system.svc" {
		t.Fatalf("names: %v", leaf.DNSNames)
	}
}

func TestCertificatesReplaceAParseableMismatchedCAKey(t *testing.T) {
	certs, c, clk := certFixture(t)
	ctx := context.Background()
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	before := secretOf(t, c)
	other, err := issue(nil, certs.DNSNames, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	corrupt := before.DeepCopy()
	corrupt.Data[keyCAKey] = other.caKeyPEM
	if err := c.Update(ctx, corrupt); err != nil {
		t.Fatal(err)
	}
	if err := certs.Ensure(ctx); err != nil {
		t.Fatalf("mismatched CA key was not repaired: %v", err)
	}
	repaired := secretOf(t, c)
	if string(repaired.Data[keyCACert]) == string(before.Data[keyCACert]) {
		t.Fatal("reused a CA whose signing key was lost")
	}
	if _, err := parse(repaired.Data); err != nil || certs.Ready(nil) != nil {
		t.Fatalf("repaired certificate is not ready: %v", err)
	}
}

type failedCAInjection struct{ client.Client }

func (c failedCAInjection) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*admissionregistrationv1.ValidatingWebhookConfiguration); ok {
		return errors.New("webhook configuration temporarily unavailable")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestCertificatesLoseReadinessUntilRotatedCAIsInjected(t *testing.T) {
	certs, c, _ := certFixture(t)
	ctx := context.Background()
	if err := certs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// Losing the CA key forces a new CA, while the API server still trusts
	// the old one. A failed injection must remove the previously ready state.
	s := secretOf(t, c)
	s.Data[keyCAKey] = []byte("lost")
	if err := c.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	certs.Client = failedCAInjection{Client: c}
	if err := certs.Ensure(ctx); err == nil {
		t.Fatal("CA injection failure was not reported")
	}
	if certs.Ready(nil) == nil {
		t.Fatal("ready while the API server does not trust the serving certificate")
	}
	certs.Client = c
	if err := certs.Ensure(ctx); err != nil || certs.Ready(nil) != nil {
		t.Fatalf("certificate did not recover after CA injection: %v", err)
	}
}
