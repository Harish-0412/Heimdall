package enginee2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/engine"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Opt in only on the dedicated kind cluster; all observations come from a real
// API server and real processes. No fake client or business data is involved.
func TestLifecycle(t *testing.T) {
	cluster := os.Getenv("HEIMDALL_E2E_CONTEXT")
	if cluster == "" {
		t.Skip("run test/e2e/engine/run.sh with Docker and kind")
	}
	if !strings.HasPrefix(cluster, "kind-") {
		t.Fatal("only dedicated kind contexts are supported")
	}
	binary, images := os.Getenv("HEIMDALL_E2E_CLI"), os.Getenv("HEIMDALL_E2E_IMAGES")
	if binary == "" || images == "" {
		t.Fatal("CLI and actual image digest manifest are required")
	}
	work := t.TempDir()
	config, err := os.ReadFile("heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(work, "heimdall.yaml")
	if err = os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*raw, cluster, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gvr := func(group, version, resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	}
	run := func(wantSuccess bool, args ...string) []byte {
		t.Helper()
		args = append(args, "--allow-context", cluster, "--context", cluster, "--format", "json", "--timeout", "6m")
		cmd := exec.Command(binary, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		b, err := cmd.Output()
		if (err == nil) != wantSuccess {
			t.Fatalf("%s: %v\n%s\n%s", strings.Join(args, " "), err, stderr.String(), b)
		}
		if wantSuccess {
			t.Log(stderr.String())
		}
		return b
	}
	states := []string{filepath.Join(work, "one.json"), filepath.Join(work, "two.json")}
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	up := func(i int, generation string) engine.Result {
		t.Helper()
		b := run(true, "up", configPath, "--state", states[i], "--repo", "local/shopflow", "--tenant", "p2", "--pr", []string{"201", "202"}[i], "--generation", generation, "--images", images, "--sha", strings.TrimSpace(string(sha)))
		var r engine.Result
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if r.Phase != "ready" {
			t.Fatalf("up: %+v", r)
		}
		return r
	}
	one := up(0, "1")
	two := up(1, "1")
	t.Cleanup(func() {
		for _, state := range states {
			cmd := exec.Command(binary, "down", "--state", state, "--context", cluster, "--allow-context", cluster, "--timeout", "2m")
			_ = cmd.Run()
		}
	})
	get := func(ns, resource, name string) string {
		t.Helper()
		group, version := "", "v1"
		if resource == "statefulsets" || resource == "deployments" {
			group = "apps"
		}
		if resource == "jobs" {
			group = "batch"
		}
		o, err := client.Resource(gvr(group, version, resource)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return string(o.GetUID())
	}
	sql := func(ns, statement string) string {
		t.Helper()
		cmd := exec.Command("kubectl", "--context", cluster, "-n", ns, "exec", "postgres-0", "--", "psql", "-U", "postgres", "-d", "app", "-At", "-v", "ON_ERROR_STOP=1", "-c", statement)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("SQL failed: %v %s", err, b)
		}
		return strings.TrimSpace(string(b))
	}
	// Reapply must retain credentials and must not restart long-running pods.
	pgUID := get(one.Namespace, "pods", "postgres-0")
	podsBefore, err := client.Resource(gvr("", "v1", "pods")).Namespace(one.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=service"})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := client.Resource(gvr("", "v1", "secrets")).Namespace(one.Namespace).Get(ctx, "heimdall-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	up(0, "1")
	if pgUID != get(one.Namespace, "pods", "postgres-0") {
		t.Fatal("idempotent apply restarted Postgres")
	}
	for _, pod := range podsBefore.Items {
		if get(one.Namespace, "pods", pod.GetName()) != string(pod.GetUID()) {
			t.Fatal("idempotent apply restarted application")
		}
	}
	// Mutate only actual PostgreSQL system metadata copied at runtime, no invented
	// customer records. Reset removes the table; the second preview is unchanged.
	if sql(one.Namespace, "SELECT count(*) FROM products") != "0" || sql(two.Namespace, "SELECT count(*) FROM products") != "0" {
		t.Fatal("schema-only previews must have no seeded products")
	}
	sql(one.Namespace, "CREATE TABLE runtime_catalog AS SELECT oid, datname FROM pg_database")
	if sql(two.Namespace, "SELECT to_regclass('public.runtime_catalog') IS NULL") != "t" {
		t.Fatal("cross-preview database contamination")
	}
	run(true, "reset", "--state", states[0], "--nonce", "1")
	if sql(one.Namespace, "SELECT to_regclass('public.runtime_catalog') IS NULL") != "t" {
		t.Fatal("reset did not restore the baseline")
	}
	resetJob := get(one.Namespace, "jobs", "heimdall-reset-1-g1")
	run(true, "reset", "--state", states[0], "--nonce", "1")
	if resetJob != get(one.Namespace, "jobs", "heimdall-reset-1-g1") {
		t.Fatal("reset nonce replay reran database reset")
	}
	// Storage replacement and old-generation pruning are measured by live UIDs.
	oldSet := get(one.Namespace, "statefulsets", "postgres")
	oldClaim := get(one.Namespace, "persistentvolumeclaims", "data-postgres-0")
	resized := strings.Replace(string(config), "storage: 1Gi", "storage: 2Gi", 1)
	if err = os.WriteFile(configPath, []byte(resized), 0o600); err != nil {
		t.Fatal(err)
	}
	up(0, "2")
	if oldSet == get(one.Namespace, "statefulsets", "postgres") || oldClaim == get(one.Namespace, "persistentvolumeclaims", "data-postgres-0") {
		t.Fatal("storage change did not recreate StatefulSet and PVC")
	}
	currentCredentials, err := client.Resource(gvr("", "v1", "secrets")).Namespace(one.Namespace).Get(ctx, "heimdall-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(credentials.Object["data"])
	b, _ := json.Marshal(currentCredentials.Object["data"])
	if string(a) != string(b) {
		t.Fatal("credentials rotated during redeployment")
	}
	for _, resource := range []string{"jobs", "configmaps"} {
		group := ""
		if resource == "jobs" {
			group = "batch"
		}
		items, err := client.Resource(gvr(group, "v1", resource)).Namespace(one.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "heimdall.dev/generation=1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items.Items) > 0 {
			t.Fatalf("older %s were not pruned", resource)
		}
	}
	run(false, "up", configPath, "--state", states[0], "--repo", "local/shopflow", "--tenant", "p2", "--pr", "201", "--generation", "1", "--images", images, "--sha", strings.TrimSpace(string(sha)))
	// Restore accepted local intent after the deliberately stale request.
	up(0, "2")
	// Fail a real smoke Job. No old-generation evidence may be pruned until
	// a later generation succeeds.
	broken := strings.Replace(resized, "curl --fail http://api:8080/health", "exit 1", 1)
	if err = os.WriteFile(configPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	run(false, "up", configPath, "--state", states[0], "--repo", "local/shopflow", "--tenant", "p2", "--pr", "201", "--generation", "3", "--images", images, "--sha", strings.TrimSpace(string(sha)))
	get(one.Namespace, "jobs", "heimdall-db-prepare-g2")
	get(one.Namespace, "jobs", "heimdall-smoke-api-health-g3")
	// Shrink storage and remove a workload. Both changes require actual cleanup.
	withoutWorker := strings.Replace(string(config), "workers:\n  notifications:\n    build: {context: examples/shopflow/api}\n    command: npm run worker\n    dependsOn: [api, postgres, rabbitmq]\n", "", 1)
	if withoutWorker == string(config) {
		t.Fatal("workload removal did not change configuration")
	}
	if err = os.WriteFile(configPath, []byte(withoutWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	actualImages, err := os.ReadFile(images)
	if err != nil {
		t.Fatal(err)
	}
	var remainingImages map[string]string
	if err = json.Unmarshal(actualImages, &remainingImages); err != nil {
		t.Fatal(err)
	}
	delete(remainingImages, "notifications")
	actualImages, err = json.Marshal(remainingImages)
	if err != nil {
		t.Fatal(err)
	}
	images = filepath.Join(work, "remaining-images.json")
	if err = os.WriteFile(images, actualImages, 0o600); err != nil {
		t.Fatal(err)
	}
	up(0, "4")
	if _, err = client.Resource(gvr("apps", "v1", "deployments")).Namespace(one.Namespace).Get(ctx, "notifications", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("removed workload was not pruned: %v", err)
	}
	run(true, "status", "--state", states[0])
	run(true, "logs", "--state", states[0], "--workload", "api", "--tail", "5")
	run(true, "down", "--state", states[0])
	_, err = client.Resource(gvr("", "v1", "namespaces")).Get(ctx, one.Namespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("destroyed namespace still exists: %v", err)
	}
	if sql(two.Namespace, "SELECT count(*) FROM products") != "0" {
		t.Fatal("destroy affected the other preview")
	}
	run(true, "down", "--state", states[0])
	run(true, "down", "--state", states[1])
	t.Logf("live lifecycle validation finished at %s", time.Now().UTC().Format(time.RFC3339))
}
