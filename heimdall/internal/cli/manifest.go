package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const manifestUsage = `Usage: heimdall manifest [flags] [file]

Print a PreviewEnvironment (heimdall.dev/v1alpha1) for one pull request, for
kubectl apply against a cluster running the Heimdall agent (docs/agent.md).
The heimdall.yaml is validated, rendered once as a check, and embedded with
its SHA-256. Images must be pinned by digest.

Run it again with a higher --generation to roll out a change (new commit,
config or images), and with a higher --reset-nonce to reset the preview's
data. The agent checks the object again against its own policy at admission.

Flags:
`

func runManifest(args []string, stdout, stderr io.Writer) int {
	var f renderFlags
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, manifestUsage)
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "object name (default: the environment id)")
	namespace := fs.String("namespace", "heimdall-system", "the agent's namespace")
	resetNonce := fs.Int64("reset-nonce", 0, "reset the data whenever this increases")
	destroyed := fs.Bool("destroyed", false, "desiredState Destroyed: remove the preview but keep the object")
	dataConfigMap := fs.String("data-configmap", "", "ConfigMap in the agent's namespace holding the approved import")
	dataKey := fs.String("data-key", "", "key of the approved import in --data-configmap")
	approval := fs.String("seed-approval", "", "operator JSON approval of the sanitised import (ADR 0009)")
	format := fs.String("format", "yaml", "output format: yaml or json")
	fs.StringVar(&f.tenant, "tenant", "local", "tenant slug")
	fs.StringVar(&f.repo, "repo", "", `repository as owner/name (default "local/<config directory name>")`)
	fs.IntVar(&f.pr, "pr", 1, "pull request number")
	fs.StringVar(&f.sha, "sha", "", "full commit SHA (default git HEAD)")
	fs.Int64Var(&f.generation, "generation", 1, "deployment generation; increase it whenever inputs change")
	fs.StringVar(&f.envID, "env-id", "", "environment id (default <tenant>-<repo name>-pr<N>)")
	fs.StringVar(&f.owner, "owner", "local", "GitHub login of the pull request's author")
	fs.StringVar(&f.expires, "expires", "", "expiry as RFC 3339 (default now + preview.ttl)")
	fs.StringVar(&f.suffix, "suffix", "", "hostname suffix, 4-8 of [a-z0-9] (default derived from tenant, repo and PR)")
	fs.Var(&f.images, "image", "workload=digest-pinned image (repeatable)")
	fs.StringVar(&f.imagesFile, "images", "", "JSON file mapping workloads to digest-pinned images")
	fs.StringVar(&f.repoRoot, "repo-root", "", "root for the repository-relative import (default the config's directory)")
	f.baseDomain, f.scheme, f.gateway = "localtest.me", "https", render.DefaultGatewayNamespace+"/"+render.DefaultGatewayName
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if len(pos) > 1 || (*format != "yaml" && *format != "json") {
		fs.Usage()
		return ExitUsage
	}
	file := defaultConfigFile
	if len(pos) == 1 {
		file = pos[0]
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "heimdall:", err); return ExitInvalid }

	policy := config.DefaultPolicy()
	cfg, code := loadConfig(file, policy, stderr)
	if cfg == nil {
		return code
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fail(err)
	}
	if f.sha == "" {
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = filepath.Dir(file)
		b, err := cmd.Output()
		if err != nil {
			return fail(fmt.Errorf("--sha is required outside a Git checkout"))
		}
		f.sha = strings.TrimSpace(string(b))
	}
	rc, err := f.context(file, cfg, policy)
	if err != nil {
		return fail(err)
	}
	// The same render the agent performs, so unpinned images and other
	// errors surface here rather than at admission.
	if _, err := render.Render(cfg, rc); err != nil {
		return fail(err)
	}

	sum := sha256.Sum256(raw)
	pe := &v1alpha1.PreviewEnvironment{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "PreviewEnvironment"},
		ObjectMeta: metav1.ObjectMeta{Name: rc.EnvironmentID, Namespace: *namespace},
		Spec: v1alpha1.PreviewEnvironmentSpec{
			Tenant: rc.Tenant, Repository: rc.Repo, PullRequest: int64(rc.PR), Commit: rc.SHA,
			Generation: rc.Generation, EnvironmentID: rc.EnvironmentID, Owner: rc.Owner, URLSuffix: rc.URLSuffix,
			ExpiresAt: metav1.NewTime(rc.ExpiresAt), DesiredState: v1alpha1.DesiredRunning, ResetNonce: *resetNonce,
			Config: v1alpha1.ConfigSource{Inline: string(raw), SHA256: hex.EncodeToString(sum[:])},
			Images: rc.Images,
		},
	}
	if *name != "" {
		pe.Name = *name
	}
	if *destroyed {
		pe.Spec.DesiredState = v1alpha1.DesiredDestroyed
	}
	if len(pe.Spec.Images) == 0 {
		pe.Spec.Images = nil
	}
	declared := cfg.Dependencies.Postgres != nil && cfg.Dependencies.Postgres.Seed != ""
	switch {
	case declared && (*dataConfigMap == "" || *dataKey == "" || *approval == ""):
		return fail(fmt.Errorf("the config declares an import: --data-configmap, --data-key and --seed-approval are required"))
	case !declared && (*dataConfigMap != "" || *dataKey != "" || *approval != ""):
		return fail(fmt.Errorf("--data-configmap, --data-key and --seed-approval need an import declared in the config"))
	case declared:
		b, err := os.ReadFile(*approval)
		if err != nil {
			return fail(err)
		}
		var a engine.SeedApproval
		if err := json.Unmarshal(b, &a); err != nil {
			return fail(fmt.Errorf("--seed-approval: %w", err))
		}
		// The local copy must be what was approved; the agent checks the
		// ConfigMap's bytes against the same digest before importing.
		if seed := sha256.Sum256(rc.Seed); hex.EncodeToString(seed[:]) != a.SHA256 {
			return fail(fmt.Errorf("--seed-approval does not match %s", cfg.Dependencies.Postgres.Seed))
		}
		pe.Spec.Data = &v1alpha1.DataSource{ConfigMap: *dataConfigMap, Key: *dataKey, Approval: v1alpha1.DataApproval{
			SHA256: a.SHA256, ApprovedBy: a.ApprovedBy, Reason: a.Reason, Sanitised: a.Sanitised}}
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pe)
	if err != nil {
		return fail(err)
	}
	delete(obj, "status")
	delete(obj["metadata"].(map[string]any), "creationTimestamp")
	var out []byte
	if *format == "json" {
		out, err = json.MarshalIndent(obj, "", "  ")
		out = append(out, '\n')
	} else {
		out, err = yaml.Marshal(obj)
	}
	if err != nil {
		return fail(err)
	}
	_, err = stdout.Write(out)
	if err != nil {
		return fail(err)
	}
	return ExitOK
}
