// Package agente2e proves the Phase 3 exit criteria against the real agent
// image and chart on kind (run.sh). Every observation comes from the live API
// server; nothing is faked.
package agente2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const (
	agentNS     = "heimdall-system"
	release     = "heimdall-agent"
	leaseName   = "heimdall-agent.heimdall.dev"
	readyWithin = 10 * time.Minute
	goneWithin  = 5 * time.Minute
	grace       = 90 * time.Second // values.yaml sweeper.gracePeriod
	sweepEvery  = 10 * time.Second // values.yaml sweeper.interval
)

type env struct {
	t       *testing.T
	ctx     string
	c       client.Client
	cli     string
	images  string
	sha     string
	chart   string
	config  string
	started time.Time
}

func setup(t *testing.T) *env {
	kubeContext := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if kubeContext == "" {
		t.Skip("run test/e2e/agent/run.sh (Docker and kind)")
	}
	if !strings.HasPrefix(kubeContext, "kind-") {
		t.Fatal("only dedicated kind contexts are supported")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile("heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: kubeContext, c: c, cli: os.Getenv("HEIMDALL_E2E_CLI"), images: os.Getenv("HEIMDALL_E2E_IMAGES"),
		sha: os.Getenv("HEIMDALL_E2E_SHA"), chart: os.Getenv("HEIMDALL_E2E_CHART"), config: string(config), started: time.Now()}
	if e.cli == "" || e.images == "" || e.sha == "" || e.chart == "" {
		t.Fatal("run.sh sets HEIMDALL_E2E_CLI, _IMAGES, _SHA and _CHART")
	}
	t.Cleanup(func() {
		if t.Failed() {
			e.diagnostics()
		}
	})
	return e
}

// TestExitCriteria runs the scenarios in order; they share one cluster.
func TestExitCriteria(t *testing.T) {
	e := setup(t)
	steps := []struct {
		name string
		fn   func(*env)
	}{
		{"admission rejects bad input", (*env).admission},
		{"apply, roll out, reset, delete", (*env).lifecycle},
		{"failures are diagnosed in status", (*env).diagnosed},
		{"newer generation cancels older", (*env).fencing},
		{"leader killed in every stage converges", (*env).chaos},
		{"orphan removed only after grace", (*env).sweepAfterGrace},
		{"no sweeping while the source is down; configmap source", (*env).sourceDown},
		{"metrics require authentication", (*env).metrics},
	}
	for _, s := range steps {
		if !t.Run(s.name, func(t *testing.T) {
			sub := *e
			sub.t = t
			s.fn(&sub)
		}) {
			t.FailNow()
		}
	}
}

// --- scenarios -------------------------------------------------------------

func (e *env) admission() {
	valid := e.manifest("pr-391", e.config, "--pr", "391")
	e.admitted(valid)

	// An invalid heimdall.yaml with a correct digest: the webhook runs the
	// agent's own checks and points at the field.
	bad := strings.Replace(e.config, "port: 8080", "port: 8080\n    replicas: 3", 1)
	e.denied(e.edit(valid, func(spec map[string]any) {
		spec["config"] = map[string]any{"inline": bad, "sha256": sha256Hex(bad)}
	}), "spec.config.inline", "heimdall.yaml is invalid")

	// A config that does not match its digest.
	e.denied(e.edit(valid, func(spec map[string]any) {
		spec["config"].(map[string]any)["sha256"] = sha256Hex("something else")
	}), "spec.config.inline", "does not match spec.config.sha256")

	// An image that is not pinned by digest never reaches the cluster.
	e.denied(e.edit(valid, func(spec map[string]any) {
		spec["images"].(map[string]any)["api"] = "localhost:5003/shopflow-api:e2e"
	}), "spec.config.inline", "pinned by digest")

	// Sleeping arrives in P8; the API refuses it until then.
	e.denied(e.edit(valid, func(spec map[string]any) { spec["desiredState"] = "Sleeping" }), "spec.desiredState", "Unsupported value")
	e.t.Log("invalid configs, digest mismatches, unpinned images and unsupported states are rejected at admission")
}

// edit applies fn to a manifest's spec.
func (e *env) edit(manifest string, fn func(spec map[string]any)) string {
	e.t.Helper()
	var obj map[string]any
	e.must(yaml.Unmarshal([]byte(manifest), &obj))
	fn(obj["spec"].(map[string]any))
	out, err := yaml.Marshal(obj)
	e.must(err)
	return string(out)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (e *env) lifecycle() {
	name := "pr-301"
	e.apply(e.manifest(name, e.config, "--pr", "301", "--generation", "1"))
	pe := e.waitReady(name, 1)
	ns := pe.Status.Namespace
	if len(pe.Status.URLs) == 0 || !pe.Status.URLs[0].Primary {
		e.t.Errorf("status URLs: %+v", pe.Status.URLs)
	}
	for _, c := range pe.Status.Conditions {
		if c.Status != metav1.ConditionTrue && c.Type != v1alpha1.ConditionProgressing {
			e.t.Errorf("condition %s is %s: %s", c.Type, c.Status, c.Message)
		}
	}
	var namespace corev1.Namespace
	e.must(e.c.Get(context.Background(), types.NamespacedName{Name: ns}, &namespace))
	if namespace.Labels[render.LabelPreview] != "true" || namespace.Labels[render.LabelPR] != "301" {
		e.t.Errorf("namespace labels: %v", namespace.Labels)
	}
	e.t.Logf("%s is Ready in %s with %s", name, ns, pe.Status.URLs[0].URL)

	// Status is the agent's record: nobody else may write it.
	e.kubectlFails("Heimdall agent alone", "-n", agentNS, "patch", "previewenvironment", name, "--subresource=status",
		"--type=merge", "-p", `{"status":{"deployedGeneration":99}}`)
	// The API refuses stale and silently changed specs (CEL rules).
	e.kubectlFails("must not decrease", "-n", agentNS, "patch", "previewenvironment", name, "--type=merge",
		"--dry-run=server", "-p", `{"spec":{"generation":0}}`)
	e.kubectlFails("require a higher spec.generation", "-n", agentNS, "patch", "previewenvironment", name, "--type=merge",
		"--dry-run=server", "-p", `{"spec":{"commit":"`+strings.Repeat("b", 40)+`"}}`)

	// Spec change: a new generation rolls out, and the old one is pruned.
	changed := strings.Replace(e.config, "    health: {path: /health}\n", "    health: {path: /health}\n    env: {E2E_GENERATION: \"2\"}\n", 1)
	e.apply(e.manifest(name, changed, "--pr", "301", "--generation", "2"))
	e.waitReady(name, 2)
	e.noneLabelled(ns, "heimdall.dev/generation=1")

	// Reset: same generation, higher nonce.
	e.apply(e.manifest(name, changed, "--pr", "301", "--generation", "2", "--reset-nonce", "1"))
	e.wait(name, "reset completed", readyWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.CompletedResetNonce == 1 && pe.Status.Phase == v1alpha1.PhaseReady
	})

	e.deleteAndGone(name, ns)
}

// notNullMigration adds a NOT NULL column without a default to a table with
// rows: PostgreSQL's own catalog, copied at runtime (no invented records).
const notNullMigration = `migrations:
  service: api
  command: >-
    node --input-type=module -e "import pg from 'pg';
    const c = new pg.Client({connectionString: process.env.DATABASE_URL}); await c.connect();
    await c.query('CREATE TABLE IF NOT EXISTS catalog_snapshot AS SELECT datname FROM pg_database');
    await c.query('ALTER TABLE catalog_snapshot ADD COLUMN owner_id integer NOT NULL');
    await c.end();"
`

func (e *env) diagnosed() {
	name := "pr-305"
	broken := strings.Replace(e.config, "migrations:\n  service: api\n  command: npm run migrate\n", notNullMigration, 1)
	if broken == e.config {
		e.t.Fatal("migration edit did not apply")
	}
	e.apply(e.manifest(name, broken, "--pr", "305", "--generation", "1"))
	pe := e.wait(name, "Failed with a diagnosis", readyWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseFailed && len(pe.Status.Diagnoses) > 0
	})
	d := pe.Status.Diagnoses[0]
	if d.Code != "MIGRATION_FAILED" || !strings.Contains(d.Summary, "SQLSTATE 23502") || !strings.Contains(d.Suggestion, "DEFAULT") || len(d.Evidence) == 0 {
		e.t.Fatalf("diagnosis: %+v", d)
	}
	ready := false
	for _, c := range pe.Status.Conditions {
		ready = ready || c.Type == v1alpha1.ConditionReady && strings.HasPrefix(c.Message, "MIGRATION_FAILED: ")
	}
	if !ready || !e.event(name, "Diagnosed") {
		e.t.Errorf("Ready message or Diagnosed event missing: %+v", pe.Status.Conditions)
	}
	e.t.Logf("%s: %s -> %s", d.Code, d.Summary, d.Suggestion)

	// The fix, as a new generation: Ready, and the diagnosis is gone.
	e.apply(e.manifest(name, e.config, "--pr", "305", "--generation", "2"))
	pe = e.waitReady(name, 2)
	if len(pe.Status.Diagnoses) != 0 {
		e.t.Errorf("diagnoses outlived the fix: %+v", pe.Status.Diagnoses)
	}
	e.deleteAndGone(name, pe.Status.Namespace)
}

func (e *env) fencing() {
	name := "pr-302"
	e.apply(e.manifest(name, e.config, "--pr", "302", "--generation", "1"))
	e.waitStage(name, 1, "dependencies", time.Time{})
	changed := strings.Replace(e.config, "    health: {path: /health}\n", "    health: {path: /health}\n    env: {E2E_GENERATION: \"2\"}\n", 1)
	e.apply(e.manifest(name, changed, "--pr", "302", "--generation", "2"))
	pe := e.waitReady(name, 2)
	if !e.event(name, "Superseded") {
		e.t.Error("no Superseded event: generation 1 was not cancelled")
	}
	e.noneLabelled(pe.Status.Namespace, "heimdall.dev/generation=1")
	e.t.Log("generation 2 cancelled generation 1; only generation 2 objects remain")
	e.deleteAndGone(name, pe.Status.Namespace)
}

func (e *env) chaos() {
	name := "pr-303"
	manifest := e.manifest(name, e.config, "--pr", "303", "--generation", "1")
	var desired v1alpha1.PreviewEnvironment
	e.must(yaml.Unmarshal([]byte(manifest), &desired))
	gate := e.chaosGate(&desired)
	gate(stageOrder[0])
	e.apply(manifest)
	previousAttempt := time.Time{}
	for i, stage := range stageOrder {
		// Admission holds this exact stage long enough to observe its engine
		// event. Arm the next stage before killing, so even a fast resumed
		// apply cannot outrun the following observation.
		e.waitStage(name, 1, stage, previousAttempt)
		next := "disabled"
		if i+1 < len(stageOrder) {
			next = stageOrder[i+1]
		}
		gate(next)
		// Recheck after activating the following hold: the kill must still
		// interrupt this stage, rather than a completed or failed attempt.
		held := e.waitStage(name, 1, stage, previousAttempt)
		previousAttempt = held.Status.Operation.StartedAt.Time
		e.killLeader(i%2 == 0, "during "+stage)
	}
	pe := e.waitReady(name, 1)
	e.t.Logf("converged after five kills; attempts recorded: %d", pe.Status.Operation.Attempts)

	// Hold a real delete-and-wait with a test-owned finalizer. PhaseDestroying
	// alone can be observed after cleanup finished and proves no interruption.
	var jobs batchv1.JobList
	e.must(e.c.List(context.Background(), &jobs, client.InNamespace(pe.Status.Namespace),
		client.MatchingLabels{render.LabelEnv: pe.Spec.EnvironmentID}))
	if len(jobs.Items) == 0 {
		e.t.Fatal("no completed Job available to hold destroy")
	}
	job := &jobs.Items[0]
	const holdFinalizer = "e2e.heimdall.dev/hold-destroy"
	job.Finalizers = append(job.Finalizers, holdFinalizer)
	e.must(e.c.Update(context.Background(), job))
	release := func() {
		var live batchv1.Job
		err := e.c.Get(context.Background(), client.ObjectKeyFromObject(job), &live)
		if apierrors.IsNotFound(err) {
			return
		}
		e.must(err)
		keep := live.Finalizers[:0]
		for _, f := range live.Finalizers {
			if f != holdFinalizer {
				keep = append(keep, f)
			}
		}
		live.Finalizers = keep
		e.must(e.c.Update(context.Background(), &live))
	}
	e.t.Cleanup(release)
	e.must(e.c.Delete(context.Background(), pe))
	held := e.wait(name, "destroy waiting on the held Job", goneWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		var live batchv1.Job
		e.must(e.c.Get(context.Background(), client.ObjectKeyFromObject(job), &live))
		return !live.DeletionTimestamp.IsZero() && runningStep(pe, v1alpha1.OperationDestroy, "destroy/jobs", time.Time{})
	})
	destroyAttempt := held.Status.Operation.StartedAt.Time
	e.killLeader(true, "during destroy")
	e.wait(name, "new leader resumed the held destroy", readyWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		return runningStep(pe, v1alpha1.OperationDestroy, "destroy/jobs", destroyAttempt)
	})
	release()
	e.gone(name, pe.Status.Namespace)
}

// chaosGate installs a test-only admission delay using the already built Node
// image. It completes TLS but holds the admission response until the API
// server's 30-second webhook timeout. Only the chaos environment's selected
// stage and four rendered resource kinds match; status writes and the agent's
// own admission guard are unaffected. Failed calls are retryable, so the hold
// also survives leader failover and the engine's journal lease expiry.
func (e *env) chaosGate(pe *v1alpha1.PreviewEnvironment) func(string) {
	e.t.Helper()
	ctx := context.Background()
	const name = "heimdall-e2e-chaos-gate"
	labels := map[string]string{"app.kubernetes.io/name": name}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	e.must(err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name + "." + agentNS + ".svc"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(2 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	e.must(err)
	private, err := x509.MarshalPKCS8PrivateKey(key)
	e.must(err)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: agentNS},
		Data: map[string][]byte{"tls.crt": ca, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentNS, Labels: labels},
		Spec: corev1.PodSpec{AutomountServiceAccountToken: new(false),
			Volumes: []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret.Name}}}},
			Containers: []corev1.Container{{
				Name: "gate", Image: pe.Spec.Images["api"],
				Command:        []string{"node", "-e", `const fs=require('fs'); require('https').createServer({cert:fs.readFileSync('/gate/tls.crt'),key:fs.readFileSync('/gate/tls.key')},(req,res)=>{if(req.url==='/ready'){res.writeHead(200);res.end('ready');return;}console.log('held-admission');req.resume();}).listen(9443,'0.0.0.0');`},
				VolumeMounts:   []corev1.VolumeMount{{Name: "tls", MountPath: "/gate", ReadOnly: true}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9443), Scheme: corev1.URISchemeHTTPS}}, PeriodSeconds: 1},
			}}}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentNS},
		Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(9443)}}}}
	deny := admissionv1.Fail
	none := admissionv1.SideEffectClassNone
	timeout := int32(30)
	vwc := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name: "chaos-gate.e2e.heimdall.dev", AdmissionReviewVersions: []string{"v1"}, FailurePolicy: &deny,
			SideEffects: &none, TimeoutSeconds: &timeout,
			ClientConfig:   admissionv1.WebhookClientConfig{CABundle: ca, Service: &admissionv1.ServiceReference{Namespace: agentNS, Name: name}},
			ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{render.LabelEnv: pe.Spec.EnvironmentID, render.LabelStage: "disabled"}},
			Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{APIGroups: []string{"", "apps", "batch"}, APIVersions: []string{"v1"},
					Resources: []string{"resourcequotas", "statefulsets", "deployments", "jobs"}}}},
		}}}
	e.t.Cleanup(func() {
		for _, obj := range []client.Object{vwc, svc, pod, secret} {
			if err := client.IgnoreNotFound(e.c.Delete(ctx, obj)); err != nil {
				e.t.Errorf("remove chaos gate: %v", err)
			}
		}
	})
	e.must(e.c.Create(ctx, secret))
	e.must(e.c.Create(ctx, svc))
	e.must(e.c.Create(ctx, pod))
	e.kubectl("-n", agentNS, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=2m")
	e.must(e.c.Create(ctx, vwc))
	probe := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: name + "-probe", Namespace: agentNS,
		Labels: map[string]string{render.LabelEnv: pe.Spec.EnvironmentID, render.LabelStage: "unmatched"}}}
	// A positive control proves the ordinary dry-run request completes.
	unmatched := func() error {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		control := probe.DeepCopy()
		control.Labels[render.LabelStage] = "unmatched"
		return e.c.Create(call, control, client.DryRunAll)
	}
	e.must(unmatched())
	heldRequests := func() (int, error) {
		out, stderr, err := e.run("", "kubectl", "--context", e.ctx, "--request-timeout=5s", "-n", agentNS, "logs", "pod/"+name)
		if err != nil {
			return 0, fmt.Errorf("read chaos gate logs: %w: %s", err, strings.TrimSpace(stderr))
		}
		return strings.Count(out, "held-admission"), nil
	}
	return func(stage string) {
		e.must(e.c.Get(ctx, types.NamespacedName{Name: name}, vwc))
		vwc.Webhooks[0].ObjectSelector.MatchLabels[render.LabelStage] = stage
		e.must(e.c.Update(ctx, vwc))
		if stage == "disabled" {
			return
		}
		// Pod readiness proves the HTTPS listener serves requests, but
		// Service routing and admission configurations propagate separately.
		// Retry startup connection errors until a matching request reaches
		// the server and remains pending while a nonmatching control succeeds.
		probe.Labels[render.LabelStage] = stage
		deadline := time.Now().Add(time.Minute)
		var lastErr error
		for time.Now().Before(deadline) {
			before, err := heldRequests()
			if err != nil {
				lastErr = err
				time.Sleep(500 * time.Millisecond)
				continue
			}
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = e.c.Create(call, probe.DeepCopy(), client.DryRunAll)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				after, logErr := heldRequests()
				if logErr != nil {
					err = logErr
				} else if after > before {
					if err = unmatched(); err == nil {
						return
					}
				}
			}
			if err == nil {
				err = errors.New("matching dry run was admitted before the gate configuration propagated")
			}
			lastErr = err
			time.Sleep(500 * time.Millisecond)
		}
		e.t.Fatalf("admission delay for %s did not become active within a minute (last error: %v)", stage, lastErr)
	}
}

func (e *env) sweepAfterGrace() {
	orphan := e.orphan("heimdall-9001-orphan-a")
	created := orphan.CreationTimestamp.Time
	// Present for the whole grace period...
	for time.Since(created) < grace-5*time.Second {
		if !e.exists(orphan.Name) {
			e.t.Fatalf("orphan deleted after %v, before the %v grace period", time.Since(created).Round(time.Second), grace)
		}
		time.Sleep(2 * time.Second)
	}
	// ...and gone within two passes after it.
	deadline := created.Add(grace + 3*sweepEvery + time.Minute)
	for e.exists(orphan.Name) {
		if time.Now().After(deadline) {
			e.t.Fatalf("orphan still present %v after creation", time.Since(created).Round(time.Second))
		}
		time.Sleep(2 * time.Second)
	}
	e.t.Logf("orphan removed %v after creation (grace %v)", time.Since(created).Round(time.Second), grace)
}

func (e *env) sourceDown() {
	// The configmap source with no ConfigMap: the source is unreachable.
	e.helm("--set", "source.type=configmap")
	orphan := e.orphan("heimdall-9002-orphan-b")
	until := orphan.CreationTimestamp.Add(grace + 6*sweepEvery)
	for time.Now().Before(until) {
		if !e.exists(orphan.Name) {
			e.t.Fatal("the sweeper deleted a namespace while the desired-state source was unreachable")
		}
		time.Sleep(3 * time.Second)
	}
	e.t.Logf("orphan kept for %v with the source down", time.Since(orphan.CreationTimestamp.Time).Round(time.Second))

	// The source comes back with one environment: the agent creates it, and
	// the orphan, now confirmed by an authoritative read, is removed.
	name := "pr-304"
	e.desiredState(name, e.manifest(name, e.config, "--pr", "304", "--generation", "1"))
	pe := e.waitReady(name, 1)
	if pe.Labels["heimdall.dev/source"] != "configmap" {
		e.t.Errorf("labels: %v", pe.Labels)
	}
	if e.exists(orphan.Name) {
		e.waitGoneNamespace(orphan.Name, 2*time.Minute)
	}
	// Only the source may change generated objects.
	e.kubectlFails("generated from the configmap desired state", "-n", agentNS, "patch", "previewenvironment", name,
		"--type=merge", "-p", `{"spec":{"resetNonce":5}}`)
	e.kubectlFails("generated from the configmap desired state", "-n", agentNS, "delete", "previewenvironment", name, "--dry-run=server")

	// Removed from the source: destroyed and released.
	e.desiredState("", "")
	e.gone(name, pe.Status.Namespace)
}

func (e *env) metrics() {
	ctx := context.Background()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "e2e-scraper", Namespace: agentNS}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "e2e-scraper"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: release + "-metrics-reader"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: agentNS}}}
	for _, o := range []client.Object{sa, binding} {
		if err := e.c.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			e.t.Fatal(err)
		}
	}
	token := strings.TrimSpace(e.kubectl("-n", agentNS, "create", "token", sa.Name, "--duration=10m"))
	leader := e.leader()
	pf := exec.Command("kubectl", "--context", e.ctx, "-n", agentNS, "port-forward", "pod/"+leader, "18443:8443")
	e.must(pf.Start())
	defer func() { _ = pf.Process.Kill(); _ = pf.Wait() }()
	// The serving certificate is self-signed by the agent; authentication is
	// the bearer token, not the certificate.
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // see above
	get := func(withToken bool) (int, string) {
		for range 20 {
			req, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1:18443/metrics", nil)
			if withToken {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := hc.Do(req)
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, string(b)
		}
		e.t.Fatal("metrics endpoint unreachable")
		return 0, ""
	}
	if code, _ := get(false); code != http.StatusUnauthorized {
		e.t.Errorf("unauthenticated scrape: HTTP %d, want 401", code)
	}
	code, body := get(true)
	if code != http.StatusOK {
		e.t.Fatalf("authorized scrape: HTTP %d", code)
	}
	for _, want := range []string{
		"heimdall_agent_admission_policy_enforced 1",
		`heimdall_agent_operations_total{result="Succeeded",type="Apply"}`,
		`heimdall_agent_operations_total{result="Succeeded",type="Destroy"}`,
		`heimdall_agent_sweeper_orphans_total{action="deleted"}`,
		`heimdall_agent_sweeper_passes_total{result="ok"}`,
		"heimdall_agent_source_up 1",
		"heimdall_agent_step_duration_seconds_bucket",
		"workqueue_depth",
	} {
		if !strings.Contains(body, want) {
			e.t.Errorf("metrics missing %s", want)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(stdin string, name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (e *env) kubectl(args ...string) string {
	e.t.Helper()
	out, errs, err := e.run("", "kubectl", append([]string{"--context", e.ctx}, args...)...)
	if err != nil {
		e.t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, errs)
	}
	return out
}

// kubectlFails runs kubectl and requires it to fail with want in its error.
func (e *env) kubectlFails(want string, args ...string) {
	e.t.Helper()
	_, errs, err := e.run("", "kubectl", append([]string{"--context", e.ctx}, args...)...)
	if err == nil {
		e.t.Fatalf("kubectl %s was allowed", strings.Join(args, " "))
	}
	if !strings.Contains(errs, want) {
		e.t.Fatalf("kubectl %s failed, but not with %q:\n%s", strings.Join(args, " "), want, errs)
	}
}

// manifest renders a PreviewEnvironment with the CLI, from config text.
func (e *env) manifest(name, config string, args ...string) string {
	e.t.Helper()
	file := e.t.TempDir() + "/heimdall.yaml"
	e.must(os.WriteFile(file, []byte(config), 0o600))
	base := []string{"manifest", file, "--name", name, "--namespace", agentNS, "--tenant", "e2e", "--repo", "local/shopflow",
		"--owner", "heimdall-e2e", "--sha", e.sha, "--images", e.images}
	out, errs, err := e.run("", e.cli, append(base, args...)...)
	if err != nil {
		e.t.Fatalf("heimdall manifest: %v\n%s", err, errs)
	}
	return out
}

func (e *env) apply(manifest string) {
	e.t.Helper()
	if _, errs, err := e.run(manifest, "kubectl", "--context", e.ctx, "apply", "-f", "-"); err != nil {
		e.t.Fatalf("kubectl apply: %v\n%s", err, errs)
	}
}

// admitted requires a server-side dry run of manifest to succeed.
func (e *env) admitted(manifest string) {
	e.t.Helper()
	if _, errs, err := e.run(manifest, "kubectl", "--context", e.ctx, "create", "--dry-run=server", "-f", "-"); err != nil {
		e.t.Fatalf("a valid PreviewEnvironment was rejected:\n%s", errs)
	}
}

// denied requires a server-side dry run of manifest to be rejected, naming
// field and containing message.
func (e *env) denied(manifest, field, message string) {
	e.t.Helper()
	_, errs, err := e.run(manifest, "kubectl", "--context", e.ctx, "create", "--dry-run=server", "-f", "-")
	if err == nil {
		e.t.Fatalf("admitted; want a rejection of %s with %q", field, message)
	}
	if !strings.Contains(errs, field) || !strings.Contains(errs, message) {
		e.t.Fatalf("rejected, but not for %s with %q:\n%s", field, message, errs)
	}
}

func (e *env) get(name string) (*v1alpha1.PreviewEnvironment, error) {
	pe := &v1alpha1.PreviewEnvironment{}
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: agentNS, Name: name}, pe)
	return pe, err
}

// wait polls until cond holds, logging each phase and step change.
func (e *env) wait(name, what string, timeout time.Duration, cond func(*v1alpha1.PreviewEnvironment) bool) *v1alpha1.PreviewEnvironment {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		pe, err := e.get(name)
		if err == nil {
			if s := describe(pe); s != last {
				e.t.Logf("  %s %s", name, s)
				last = s
			}
			if cond(pe) {
				return pe
			}
			if currentTerminalFailure(pe) {
				e.t.Fatalf("%s failed terminally: %s: %s", name, pe.Status.LastError.Code, pe.Status.LastError.Message)
			}
		} else if !apierrors.IsNotFound(err) {
			e.t.Logf("  get %s: %v", name, err)
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s: not %s within %v (last: %s)", name, what, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// A saved error can outlive the attempt that produced it, including while a
// fixed generation is starting. Only a completed failure of the currently
// reconciled intent may stop a wait early.
func currentTerminalFailure(pe *v1alpha1.PreviewEnvironment) bool {
	s := pe.Status
	o, last := s.Operation, s.LastError
	if s.Phase != v1alpha1.PhaseFailed || s.ObservedGeneration != pe.Generation ||
		last == nil || last.Retryable || last.Generation != pe.Spec.Generation ||
		o == nil || o.Result != v1alpha1.ResultFailed || o.Generation != pe.Spec.Generation {
		return false
	}
	if !pe.DeletionTimestamp.IsZero() || pe.Spec.DesiredState == v1alpha1.DesiredDestroyed {
		return o.Type == v1alpha1.OperationDestroy
	}
	return o.Type == v1alpha1.OperationApply ||
		(o.Type == v1alpha1.OperationReset && o.ResetNonce == pe.Spec.ResetNonce)
}

func TestTerminalFailureMatchesCurrentIntent(t *testing.T) {
	tests := []struct {
		name   string
		change func(*v1alpha1.PreviewEnvironment)
		want   bool
	}{
		{"current failed apply", func(*v1alpha1.PreviewEnvironment) {}, true},
		{"old generation error after fixed spec", func(pe *v1alpha1.PreviewEnvironment) { pe.Status.LastError.Generation = 1 }, false},
		{"old failed operation", func(pe *v1alpha1.PreviewEnvironment) { pe.Status.Operation.Generation = 1 }, false},
		{"spec not reconciled yet", func(pe *v1alpha1.PreviewEnvironment) { pe.Status.ObservedGeneration = 1 }, false},
		{"fixed generation provisioning with old error", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Status.Phase, pe.Status.Operation.Result = v1alpha1.PhaseProvisioning, v1alpha1.ResultRunning
			pe.Status.LastError.Generation = 1
		}, false},
		{"attempt still running", func(pe *v1alpha1.PreviewEnvironment) { pe.Status.Operation.Result = v1alpha1.ResultRunning }, false},
		{"retryable failure", func(pe *v1alpha1.PreviewEnvironment) { pe.Status.LastError.Retryable = true }, false},
		{"new reset requested", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.ResetNonce = 2
			pe.Status.Operation.Type, pe.Status.Operation.ResetNonce = v1alpha1.OperationReset, 1
		}, false},
		{"current reset failed", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.ResetNonce = 2
			pe.Status.Operation.Type, pe.Status.Operation.ResetNonce = v1alpha1.OperationReset, 2
		}, true},
		{"destroy supersedes failed apply", func(pe *v1alpha1.PreviewEnvironment) { pe.Spec.DesiredState = v1alpha1.DesiredDestroyed }, false},
		{"current destroy failed", func(pe *v1alpha1.PreviewEnvironment) {
			pe.Spec.DesiredState, pe.Status.Operation.Type = v1alpha1.DesiredDestroyed, v1alpha1.OperationDestroy
		}, true},
		{"deletion supersedes failed apply", func(pe *v1alpha1.PreviewEnvironment) { pe.DeletionTimestamp = &metav1.Time{Time: time.Now()} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &v1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec: v1alpha1.PreviewEnvironmentSpec{Generation: 2, DesiredState: v1alpha1.DesiredRunning},
				Status: v1alpha1.PreviewEnvironmentStatus{Phase: v1alpha1.PhaseFailed, ObservedGeneration: 2,
					Operation: &v1alpha1.OperationStatus{Type: v1alpha1.OperationApply, Generation: 2, Result: v1alpha1.ResultFailed},
					LastError: &v1alpha1.ErrorStatus{Code: "engine.job_failed", Generation: 2}}}
			tt.change(pe)
			if got := currentTerminalFailure(pe); got != tt.want {
				t.Fatalf("currentTerminalFailure = %v, want %v", got, tt.want)
			}
		})
	}
}

func describe(pe *v1alpha1.PreviewEnvironment) string {
	s := string(pe.Status.Phase)
	if o := pe.Status.Operation; o != nil {
		s += fmt.Sprintf(" %s(g%d)=%s", o.Type, o.Generation, o.Result)
	}
	for _, st := range pe.Status.Steps {
		if st.State == "running" {
			s += " @" + st.Name
		}
	}
	if le := pe.Status.LastError; le != nil {
		s += " [" + le.Code + "]"
	}
	return s
}

func (e *env) waitReady(name string, generation int64) *v1alpha1.PreviewEnvironment {
	e.t.Helper()
	return e.wait(name, fmt.Sprintf("Ready at generation %d", generation), readyWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseReady && pe.Status.DeployedGeneration == generation &&
			pe.Status.ObservedGeneration == pe.Generation
	})
}

var stageOrder = []string{"guardrails", "dependencies", "baseline-db", "application", "smoke"}

// waitStage requires this exact stage's engine event from the current apply.
// The chaos gate prevents fast stages from passing between observations.
func (e *env) waitStage(name string, generation int64, stage string, after time.Time) *v1alpha1.PreviewEnvironment {
	e.t.Helper()
	return e.wait(name, "running "+stage, readyWithin, func(pe *v1alpha1.PreviewEnvironment) bool {
		o := pe.Status.Operation
		if o == nil || o.Type != v1alpha1.OperationApply || o.Generation != generation || o.Result != v1alpha1.ResultRunning {
			return false
		}
		for _, st := range pe.Status.Steps {
			head, _, _ := strings.Cut(st.Name, "/")
			if head == stage && runningStep(pe, v1alpha1.OperationApply, st.Name, after) {
				return true
			}
		}
		return false
	})
}

// after is the prior attempt's recorded timestamp, not the kill's wall clock.
// Equality must fail: Kubernetes timestamps have only second precision, and
// unchanged Running status cannot prove that a replacement leader resumed.
func runningStep(pe *v1alpha1.PreviewEnvironment, operation v1alpha1.OperationType, step string, after time.Time) bool {
	o := pe.Status.Operation
	if o == nil || o.Type != operation || o.Result != v1alpha1.ResultRunning ||
		(!after.IsZero() && !o.StartedAt.After(after)) {
		return false
	}
	for _, st := range pe.Status.Steps {
		if st.Name == step && st.State == "running" && st.StartedAt != nil && !st.StartedAt.Time.Before(o.StartedAt.Time) {
			return true
		}
	}
	return false
}

func TestRunningStepRequiresFreshAttempt(t *testing.T) {
	started := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, operation := range []v1alpha1.OperationType{v1alpha1.OperationApply, v1alpha1.OperationDestroy} {
		for _, tt := range []struct {
			name  string
			after time.Time
			want  bool
		}{
			{"initial attempt", time.Time{}, true},
			{"replacement attempt", started.Add(-time.Second), true},
			{"same-second prior attempt", started, false},
			{"older attempt", started.Add(time.Second), false},
		} {
			t.Run(string(operation)+"/"+tt.name, func(t *testing.T) {
				pe := &v1alpha1.PreviewEnvironment{Status: v1alpha1.PreviewEnvironmentStatus{
					Operation: &v1alpha1.OperationStatus{Type: operation, Result: v1alpha1.ResultRunning, StartedAt: metav1.NewTime(started)},
					Steps:     []v1alpha1.StepStatus{{Name: "held", State: "running", StartedAt: &metav1.Time{Time: started}}},
				}}
				if got := runningStep(pe, operation, "held", tt.after); got != tt.want {
					t.Fatalf("runningStep = %v, want %v", got, tt.want)
				}
			})
		}
	}
}

func (e *env) leader() string {
	e.t.Helper()
	var lease coordinationv1.Lease
	e.must(e.c.Get(context.Background(), types.NamespacedName{Namespace: agentNS, Name: leaseName}, &lease))
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		e.t.Fatal("no leader")
	}
	pod, _, _ := strings.Cut(*lease.Spec.HolderIdentity, "_")
	return pod
}

// killLeader SIGKILLs the leader's process (hard) or force-deletes its pod.
func (e *env) killLeader(hard bool, when string) {
	e.t.Helper()
	name := e.leader()
	var pod corev1.Pod
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: agentNS, Name: name}, &pod); err != nil {
		e.t.Fatalf("leader %s: %v", name, err)
	}
	if hard {
		// crictl on the kind node: StopContainer with no grace is SIGKILL.
		sandbox, errs, err := e.run("", "docker", "exec", pod.Spec.NodeName, "crictl", "pods", "--name", "^"+name+"$", "-q")
		if err != nil || strings.TrimSpace(sandbox) == "" {
			e.t.Fatalf("crictl pods: %v %s", err, errs)
		}
		ids, errs, err := e.run("", "docker", "exec", pod.Spec.NodeName, "crictl", "ps", "--pod", strings.Fields(sandbox)[0], "--name", "^agent$", "-q")
		if err != nil || strings.TrimSpace(ids) == "" {
			e.t.Fatalf("crictl ps: %v %s", err, errs)
		}
		if _, errs, err := e.run("", "docker", "exec", pod.Spec.NodeName, "crictl", "stop", "--timeout", "0", strings.Fields(ids)[0]); err != nil {
			e.t.Fatalf("crictl stop: %v %s", err, errs)
		}
		e.t.Logf("SIGKILLed leader %s %s", name, when)
		return
	}
	e.must(e.c.Delete(context.Background(), &pod, client.GracePeriodSeconds(0)))
	e.t.Logf("force-deleted leader pod %s %s", name, when)
}

func (e *env) event(name, reason string) bool {
	var list eventsv1.EventList
	e.must(e.c.List(context.Background(), &list, client.InNamespace(agentNS)))
	for _, ev := range list.Items {
		if ev.Regarding.Name == name && ev.Reason == reason {
			return true
		}
	}
	return false
}

func (e *env) noneLabelled(namespace, selector string) {
	e.t.Helper()
	for _, kind := range []string{"jobs", "configmaps", "deployments", "statefulsets"} {
		if out := strings.TrimSpace(e.kubectl("-n", namespace, "get", kind, "-l", selector, "-o", "name")); out != "" {
			e.t.Errorf("%s left from an older generation: %s", kind, out)
		}
	}
}

func (e *env) exists(namespace string) bool {
	err := e.c.Get(context.Background(), types.NamespacedName{Name: namespace}, &corev1.Namespace{})
	if err != nil && !apierrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	return err == nil
}

func (e *env) waitGoneNamespace(namespace string, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for e.exists(namespace) {
		if time.Now().After(deadline) {
			e.t.Fatalf("namespace %s still exists after %v", namespace, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func (e *env) deleteAndGone(name, namespace string) {
	e.t.Helper()
	pe, err := e.get(name)
	e.must(err)
	e.must(e.c.Delete(context.Background(), pe))
	e.gone(name, namespace)
}

// gone requires the object released and its namespace deleted completely.
func (e *env) gone(name, namespace string) {
	e.t.Helper()
	start, last := time.Now(), ""
	for {
		pe, err := e.get(name)
		if apierrors.IsNotFound(err) {
			break
		}
		e.must(err)
		if s := describe(pe); s != last {
			e.t.Logf("  %s %s", name, s)
			last = s
		}
		if time.Since(start) > goneWithin {
			e.t.Fatalf("%s still exists after %v (last: %s)", name, goneWithin, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.waitGoneNamespace(namespace, goneWithin)
	e.t.Logf("%s released and namespace %s deleted in %v", name, namespace, time.Since(start).Round(time.Second))
}

func (e *env) orphan(name string) *corev1.Namespace {
	e.t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		render.LabelPreview: "true", "app.kubernetes.io/managed-by": render.ManagedBy, render.LabelTenant: "e2e",
		render.LabelRepo: "local.orphan", render.LabelPR: strings.Split(name, "-")[1], render.LabelEnv: name,
	}}}
	e.must(e.c.Create(context.Background(), ns))
	e.t.Logf("hand-labelled orphan %s created", name)
	return ns
}

func (e *env) helm(args ...string) {
	e.t.Helper()
	base := []string{"upgrade", release, e.chart, "--kube-context", e.ctx, "--namespace", agentNS, "--reuse-values", "--wait", "--timeout", "5m"}
	if _, errs, err := e.run("", "helm", append(base, args...)...); err != nil {
		e.t.Fatalf("helm upgrade: %v\n%s", err, errs)
	}
	e.kubectl("-n", agentNS, "rollout", "status", "deployment/"+release, "--timeout=5m")
}

// desiredState writes the configmap source's document with one environment
// (taken from a PreviewEnvironment manifest), or none.
func (e *env) desiredState(name, manifest string) {
	e.t.Helper()
	doc := "apiVersion: heimdall.dev/v1alpha1\nkind: DesiredState\nenvironments: []\n"
	if manifest != "" {
		var obj map[string]any
		e.must(yaml.Unmarshal([]byte(manifest), &obj))
		b, err := yaml.Marshal(map[string]any{"apiVersion": "heimdall.dev/v1alpha1", "kind": "DesiredState",
			"environments": []any{map[string]any{"name": name, "spec": obj["spec"]}}})
		e.must(err)
		doc = string(b)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "heimdall-desired-state", Namespace: agentNS},
		Data: map[string]string{"desired-state.yaml": doc}}
	err := e.c.Create(context.Background(), cm)
	if apierrors.IsAlreadyExists(err) {
		err = e.c.Update(context.Background(), cm)
	}
	e.must(err)
}

func (e *env) diagnostics() {
	e.t.Log("---- diagnostics ----")
	for _, args := range [][]string{
		{"-n", agentNS, "get", "previewenvironments,pods,leases", "-o", "wide"},
		{"-n", agentNS, "get", "previewenvironments", "-o", "yaml"},
		{"get", "namespaces", "-l", "heimdall.dev/preview=true"},
		{"-n", agentNS, "logs", "-l", "app.kubernetes.io/name=heimdall-agent", "--tail=150", "--prefix", "--all-containers"},
	} {
		out, errs, _ := e.run("", "kubectl", append([]string{"--context", e.ctx}, args...)...)
		e.t.Logf("$ kubectl %s\n%s%s", strings.Join(args, " "), out, errs)
	}
}
