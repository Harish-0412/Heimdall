package webhook

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Certificate lifetimes. The CA outlives many serving certificates; serving
// certificates are renewed well before they expire.
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	certValidity = 365 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour
)

// Keys of the certificate Secret.
const (
	keyCert   = "tls.crt"
	keyKey    = "tls.key"
	keyCACert = "ca.crt"
	keyCAKey  = "ca.key"
)

// Certificates issues and rotates the webhook's serving certificate from a
// self-managed CA kept in a Secret in the agent's namespace, serves it from
// memory (so rotation needs no restart), and keeps the CA in the
// ValidatingWebhookConfiguration's caBundle.
//
// Every replica runs it, because every replica serves the webhook whoever
// leads. Secret writes carry resourceVersion preconditions, so concurrent
// replicas converge on a single certificate.
type Certificates struct {
	Client        client.Client
	Reader        client.Reader // uncached
	Namespace     string
	SecretName    string
	WebhookConfig string
	DNSNames      []string
	Refresh       time.Duration
	Clock         clock.PassiveClock
	Log           logr.Logger

	mu       sync.RWMutex
	current  *tls.Certificate
	injected bool
}

// Start implements manager.Runnable.
func (c *Certificates) Start(ctx context.Context) error {
	if c.Refresh <= 0 {
		c.Refresh = time.Minute
	}
	for {
		wait := c.Refresh
		if err := c.Ensure(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error(err, "webhook certificate not ready")
			// Retry soon: until it succeeds this replica is not ready and
			// PreviewEnvironment writes fail closed.
			wait = min(wait, retryInterval)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

const retryInterval = 2 * time.Second

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *Certificates) NeedLeaderElection() bool { return false }

// GetCertificate serves the current certificate (tls.Config hook).
func (c *Certificates) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current == nil {
		return nil, errors.New("webhook certificate not issued yet")
	}
	return c.current, nil
}

// Ready is a readiness check: a certificate is loaded and the API server
// has been told to trust it.
func (c *Certificates) Ready(*http.Request) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current == nil || !c.injected {
		return errors.New("webhook certificate not ready")
	}
	return nil
}

// Ensure makes the Secret hold a valid certificate for DNSNames, loads it,
// and injects the CA into the webhook configuration.
func (c *Certificates) Ensure(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			c.mu.Lock()
			c.injected = false
			c.mu.Unlock()
		}
	}()
	if c.Clock == nil {
		c.Clock = clock.RealClock{}
	}
	now := c.Clock.Now()
	key := types.NamespacedName{Namespace: c.Namespace, Name: c.SecretName}
	secret := &corev1.Secret{}
	err = c.Reader.Get(ctx, key, secret)
	switch {
	case apierrors.IsNotFound(err):
		b, err := issue(nil, c.DNSNames, now)
		if err != nil {
			return err
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.SecretName, Namespace: c.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "heimdall-agent"}},
			Type: corev1.SecretTypeOpaque, Data: b.data()}
		if err := c.Client.Create(ctx, secret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("another replica issued the certificate first; reloading next pass")
			}
			return fmt.Errorf("create certificate secret: %w", err)
		}
		c.Log.Info("issued webhook CA and serving certificate", "expires", b.leaf.NotAfter)
	case err != nil:
		return fmt.Errorf("read certificate secret: %w", err)
	default:
		b, problem := parse(secret.Data)
		if problem == nil {
			problem = b.check(c.DNSNames, now)
		}
		if problem != nil {
			// Keep the CA while it remains valid long enough for a new serving
			// certificate, so the trusted caBundle does not change needlessly.
			var ca *bundle
			if b != nil && b.ca != nil && b.caKey != nil && b.ca.NotAfter.After(now.Add(certValidity+renewBefore)) {
				ca = b
			}
			fresh, err := issue(ca, c.DNSNames, now)
			if err != nil {
				return err
			}
			secret.Data = fresh.data()
			if err := c.Client.Update(ctx, secret); err != nil {
				return fmt.Errorf("renew certificate (%w): %w", problem, err)
			}
			c.Log.Info("renewed webhook serving certificate", "reason", problem.Error(), "expires", fresh.leaf.NotAfter)
		}
	}
	pair, err := tls.X509KeyPair(secret.Data[keyCert], secret.Data[keyKey])
	if err != nil {
		return fmt.Errorf("load serving certificate: %w", err)
	}
	c.mu.Lock()
	c.current = &pair
	// A previously injected CA says nothing about this certificate. Keep
	// readiness false until the API server trusts the currently served CA.
	c.injected = false
	c.mu.Unlock()
	return c.inject(ctx, secret.Data[keyCACert])
}

// inject sets caBundle on every webhook of the configuration.
func (c *Certificates) inject(ctx context.Context, ca []byte) error {
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := c.Reader.Get(ctx, types.NamespacedName{Name: c.WebhookConfig}, vwc); err != nil {
		return fmt.Errorf("read webhook configuration %s: %w", c.WebhookConfig, err)
	}
	changed := false
	for i := range vwc.Webhooks {
		if !bytes.Equal(vwc.Webhooks[i].ClientConfig.CABundle, ca) {
			vwc.Webhooks[i].ClientConfig.CABundle = ca
			changed = true
		}
	}
	if changed {
		if err := c.Client.Update(ctx, vwc); err != nil {
			return fmt.Errorf("inject caBundle: %w", err)
		}
		c.Log.Info("injected webhook CA bundle", "configuration", c.WebhookConfig)
	}
	c.mu.Lock()
	c.injected = true
	c.mu.Unlock()
	return nil
}

// bundle is a parsed CA and serving certificate with their keys.
type bundle struct {
	ca, leaf            *x509.Certificate
	caKey, leafKey      *ecdsa.PrivateKey
	caPEM, caKeyPEM     []byte
	leafPEM, leafKeyPEM []byte
}

func (b *bundle) data() map[string][]byte {
	return map[string][]byte{keyCert: b.leafPEM, keyKey: b.leafKeyPEM, keyCACert: b.caPEM, keyCAKey: b.caKeyPEM}
}

// check reports why a bundle must be renewed, or nil.
func (b *bundle) check(dnsNames []string, now time.Time) error {
	switch {
	case now.Add(renewBefore).After(b.leaf.NotAfter):
		return errors.New("serving certificate expires soon")
	case now.Add(renewBefore).After(b.ca.NotAfter):
		return errors.New("CA expires soon")
	case !slices.Equal(sorted(b.leaf.DNSNames), sorted(dnsNames)):
		return errors.New("serving names changed")
	}
	if err := b.leaf.CheckSignatureFrom(b.ca); err != nil {
		return errors.New("serving certificate is not signed by the CA")
	}
	return nil
}

func sorted(in []string) []string { return slices.Sorted(slices.Values(in)) }

// parse decodes a certificate Secret. Any malformation is reported so the
// caller reissues rather than serving something broken.
func parse(data map[string][]byte) (*bundle, error) {
	b := &bundle{caPEM: data[keyCACert], caKeyPEM: data[keyCAKey], leafPEM: data[keyCert], leafKeyPEM: data[keyKey]}
	var err error
	if b.ca, err = parseCert(b.caPEM); err != nil {
		return nil, fmt.Errorf("CA certificate: %w", err)
	}
	if b.caKey, err = parseKey(b.caKeyPEM); err != nil {
		return &bundle{ca: b.ca}, fmt.Errorf("CA key: %w", err)
	}
	if !b.caKey.PublicKey.Equal(b.ca.PublicKey) {
		// Never reuse a CA whose signing key does not match its certificate:
		// otherwise the next serving-certificate renewal cannot succeed.
		return &bundle{ca: b.ca}, errors.New("CA key does not match the CA certificate")
	}
	if b.leaf, err = parseCert(b.leafPEM); err != nil {
		return b, fmt.Errorf("serving certificate: %w", err)
	}
	if b.leafKey, err = parseKey(b.leafKeyPEM); err != nil {
		return b, fmt.Errorf("serving key: %w", err)
	}
	if _, err := tls.X509KeyPair(b.leafPEM, b.leafKeyPEM); err != nil {
		return b, fmt.Errorf("serving key pair: %w", err)
	}
	return b, nil
}

func parseCert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(p []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("not a PEM EC private key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// issue creates a serving certificate, signed by ca's CA, or by a new CA
// when ca is nil.
func issue(ca *bundle, dnsNames []string, now time.Time) (*bundle, error) {
	b := &bundle{}
	if ca != nil {
		b.ca, b.caKey, b.caPEM, b.caKeyPEM = ca.ca, ca.caKey, ca.caPEM, ca.caKeyPEM
	} else {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: "heimdall-agent-webhook-ca"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caValidity),
			IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return nil, err
		}
		if b.ca, err = x509.ParseCertificate(der); err != nil {
			return nil, err
		}
		b.caKey = key
		b.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if b.caKeyPEM, err = encodeKey(key); err != nil {
			return nil, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: dnsNames[0]}, DNSNames: dnsNames,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.ca, &key.PublicKey, b.caKey)
	if err != nil {
		return nil, err
	}
	if b.leaf, err = x509.ParseCertificate(der); err != nil {
		return nil, err
	}
	b.leafKey = key
	b.leafPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if b.leafKeyPEM, err = encodeKey(key); err != nil {
		return nil, err
	}
	return b, nil
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return n
}
