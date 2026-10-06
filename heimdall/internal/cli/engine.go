package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// The local file is desired input, not a cached cluster status. It pins the
// config, images, generation and target across up/reset/down and restarts.
type localSpec struct {
	Version     int                  `json:"version"`
	KubeContext string               `json:"kubeContext"`
	Cluster     string               `json:"cluster"`
	Config      string               `json:"config"`
	Context     render.Context       `json:"context"`
	Approval    *engine.SeedApproval `json:"seedApproval,omitempty"`
}

func clusterConfig(kubeconfig, selected string, allow []string) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	raw, err := rules.Load()
	if err != nil {
		return nil, "", err
	}
	if selected == "" {
		selected = raw.CurrentContext
	}
	if selected == "" || !slices.Contains(allow, selected) {
		return nil, "", fmt.Errorf("engine.context_denied: kube-context must appear in an explicit --allow-context entry")
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, selected, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, "", err
	}
	cfg.UserAgent = "heimdall-engine"
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.Timeout = 30 * time.Second
	return cfg, selected, nil
}
func clusterIdentity(c *rest.Config) (string, error) {
	ca := c.CAData
	if len(ca) == 0 && c.CAFile != "" {
		var err error
		ca, err = os.ReadFile(c.CAFile)
		if err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(append([]byte(c.Host+"\n"+c.ServerName+fmt.Sprint(c.Insecure)), ca...))
	return hex.EncodeToString(sum[:]), nil
}
func saveLocal(path string, s localSpec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".intent-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
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
	return os.Rename(name, path)
}
func loadLocal(path string) (localSpec, error) {
	var s localSpec
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	err = d.Decode(&s)
	if err == nil && s.Version != 1 {
		err = fmt.Errorf("unsupported local state version")
	}
	return s, err
}

func runEngine(command string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", ".heimdall/environment.json", "local desired-state file (use a separate file per environment)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default Kubernetes loading rules)")
	kubeContext := fs.String("context", "", "target kube-context (default current context on up; saved context otherwise)")
	var allow listFlag
	fs.Var(&allow, "allow-context", "exact trusted kube-context; repeatable and required for every cluster command")
	timeout := fs.Duration("timeout", 10*time.Minute, "deadline for each engine step")
	format := fs.String("format", "text", "output format: text or json")
	nonce := fs.Int64("nonce", 0, "positive monotonic reset operation nonce")
	workload := fs.String("workload", "", "logs: service, worker or Job pod label")
	container := fs.String("container", "", "logs: container name (default all regular containers)")
	tail := fs.Int64("tail", 100, "logs: lines per container, 1-10000")
	follow := fs.Bool("follow", false, "logs: follow one selected pod/container")
	reason := fs.String("reason", "", "force-cleanup: required audit reason")
	evidence := fs.String("evidence", "", "force-cleanup: new local evidence file written before deletion")
	approval := fs.String("seed-approval", "", "operator JSON approval bound to the SHA-256 of sanitised data; synthetic data is prohibited")
	var f renderFlags
	fs.StringVar(&f.tenant, "tenant", "local", "tenant slug")
	fs.StringVar(&f.repo, "repo", "", "repository as owner/name")
	fs.IntVar(&f.pr, "pr", 1, "pull request number")
	fs.StringVar(&f.sha, "sha", "", "commit SHA (default git HEAD)")
	fs.Int64Var(&f.generation, "generation", 1, "explicit deployment generation; increment on changes")
	fs.StringVar(&f.envID, "env-id", "", "environment ID")
	fs.StringVar(&f.owner, "owner", "local", "PR owner")
	fs.StringVar(&f.expires, "expires", "", "expiry as RFC3339")
	fs.StringVar(&f.suffix, "suffix", "", "stable hostname suffix")
	fs.Var(&f.images, "image", "workload=digest-pinned image (repeatable)")
	fs.StringVar(&f.imagesFile, "images", "", "JSON workload-to-image mapping")
	fs.StringVar(&f.repoRoot, "repo-root", "", "root for repository-relative approved data")
	fs.StringVar(&f.baseDomain, "base-domain", "localtest.me", "preview DNS zone")
	fs.StringVar(&f.scheme, "scheme", "http", "preview URL scheme")
	fs.IntVar(&f.urlPort, "url-port", 0, "preview URL port")
	fs.StringVar(&f.gateway, "gateway", render.DefaultGatewayNamespace+"/"+render.DefaultGatewayName, "Gateway namespace/name[/listener]")
	fs.StringVar(&f.storageClass, "storage-class", "", "Postgres storage class")
	fs.Var(&f.egress, "egress-cidr", "operator-controlled egress CIDR (repeatable)")
	fs.StringVar(&f.imageMirror, "image-mirror", "", "dependency image mirror")
	fs.Var(&f.nodeSelector, "node-selector", "key=value label of the preview node pool (repeatable)")
	saveSnapshot := fs.String("save-snapshot", "", "up/reset: when the operation fails, also write the diagnosis snapshot (JSON, redacted) here")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if *timeout <= 0 || (*format != "text" && *format != "json") || len(pos) > 1 || (command != "up" && len(pos) > 0) {
		fmt.Fprintln(stderr, "heimdall: invalid arguments; use", command, "-h")
		return ExitUsage
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "heimdall:", err); return ExitInvalid }
	var local localSpec
	if command != "up" {
		local, err = loadLocal(*state)
		if err != nil {
			return fail(err)
		}
		if *kubeContext == "" {
			*kubeContext = local.KubeContext
		}
	}
	restConfig, selected, err := clusterConfig(*kubeconfig, *kubeContext, allow)
	if err != nil {
		return fail(err)
	}
	cluster, err := clusterIdentity(restConfig)
	if err != nil {
		return fail(err)
	}
	if command != "up" && (local.KubeContext != selected || local.Cluster != cluster) {
		return fail(fmt.Errorf("engine.cluster_mismatch: saved intent belongs to a different cluster/context"))
	}
	policy := config.DefaultPolicy()
	var cfg *config.Config
	if command == "up" {
		file := defaultConfigFile
		if len(pos) == 1 {
			file = pos[0]
		}
		var code int
		cfg, code = loadConfig(file, policy, stderr)
		if cfg == nil {
			return code
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
		b, err := os.ReadFile(file)
		if err != nil {
			return fail(err)
		}
		local = localSpec{Version: 1, KubeContext: selected, Cluster: cluster, Config: string(b), Context: rc}
		if *approval != "" {
			b, err := os.ReadFile(*approval)
			if err != nil {
				return fail(err)
			}
			if err = json.Unmarshal(b, &local.Approval); err != nil {
				return fail(fmt.Errorf("invalid seed approval"))
			}
		}
		if previous, err := loadLocal(*state); err == nil {
			if previous.Cluster != cluster || previous.KubeContext != selected || previous.Context.EnvironmentID != rc.EnvironmentID || previous.Context.Repo != rc.Repo || previous.Context.URLSuffix != rc.URLSuffix || previous.Context.PR != rc.PR || previous.Context.Tenant != rc.Tenant {
				return fail(fmt.Errorf("engine.state_mismatch: use a separate --state file for this environment"))
			}
			if rc.Generation < previous.Context.Generation {
				return fail(fmt.Errorf("engine.stale_generation: generation is older than saved intent"))
			}
			if rc.Generation == previous.Context.Generation {
				previousConfig, _ := config.Load(strings.NewReader(previous.Config), policy)
				if previousConfig == nil {
					return fail(fmt.Errorf("saved configuration is invalid"))
				}
				oldDigest, err := (engine.Spec{Config: previousConfig, Context: previous.Context, SeedApproval: previous.Approval}).Digest()
				if err != nil {
					return fail(err)
				}
				newDigest, err := (engine.Spec{Config: cfg, Context: rc, SeedApproval: local.Approval}).Digest()
				if err != nil {
					return fail(err)
				}
				if oldDigest != newDigest {
					return fail(fmt.Errorf("engine.generation_conflict: changed inputs require a new generation"))
				}
				if f.expires == "" {
					local.Context.ExpiresAt = previous.Context.ExpiresAt
				}
			}
		} else if !os.IsNotExist(err) {
			return fail(err)
		}
	} else {
		var diagnostics config.Diagnostics
		cfg, diagnostics = config.Load(strings.NewReader(local.Config), policy)
		if cfg == nil {
			writeText(stderr, *state, diagnostics)
			return ExitInvalid
		}
	}
	local.Context.Policy = policy
	spec := engine.Spec{Config: cfg, Context: local.Context, SeedApproval: local.Approval}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fail(err)
	}
	kubernetes := &engine.Kubernetes{Client: client}
	observer := func(ev engine.Event) {
		if *format == "text" {
			fmt.Fprintf(stderr, "%s %s %s (%s)\n", ev.Operation, ev.Stage, ev.State, ev.Duration.Round(time.Millisecond))
		}
	}
	eng := engine.New(kubernetes, *timeout, observer)
	if _, _, err = eng.Scope(spec); err != nil {
		// Refused before any change (an import that is not approved, a spec
		// that does not render): still explained.
		if command == "up" || command == "reset" {
			explainFailure(restConfig, local.Context, err, *saveSnapshot, stderr)
		}
		return fail(err)
	}
	if command == "up" {
		if err = saveLocal(*state, local); err != nil {
			return fail(err)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var result any
	switch command {
	case "up":
		result, err = eng.Apply(ctx, spec)
	case "down":
		result, err = eng.Destroy(ctx, spec)
	case "reset":
		result, err = eng.Reset(ctx, spec, *nonce)
	case "status":
		result, err = eng.Status(ctx, spec)
	case "logs":
		err = writeLogs(ctx, restConfig, eng, spec, *workload, *container, *tail, *follow, stdout)
	case "force-cleanup":
		err = forceCleanup(ctx, restConfig, eng, spec, *reason, *evidence, *timeout)
		if err == nil {
			result = map[string]string{"phase": "force-cleaned", "evidence": *evidence}
		}
	}
	if result != nil {
		if encodeErr := writeEngineResult(stdout, *format, result); encodeErr != nil {
			return fail(encodeErr)
		}
	}
	if err != nil && (command == "up" || command == "reset") && ctx.Err() == nil {
		explainFailure(restConfig, local.Context, err, *saveSnapshot, stderr)
	}
	if err != nil {
		return fail(err)
	}
	return ExitOK
}

func writeEngineResult(out io.Writer, format string, value any) error {
	if format == "json" {
		return json.NewEncoder(out).Encode(value)
	}
	var result *engine.Result
	switch v := value.(type) {
	case *engine.Result:
		result = v
	case *engine.Status:
		if v == nil {
			return nil
		}
		result = &v.Result
		for _, o := range v.Objects {
			state := "waiting"
			if o.Ready {
				state = "ready"
			}
			if o.Code != "" {
				state = o.Code
			}
			if _, err := fmt.Fprintf(out, "%-16s %-48s %s\n", o.Kind, o.Name, state); err != nil {
				return err
			}
		}
	default:
		return json.NewEncoder(out).Encode(value)
	}
	if result == nil {
		return nil
	}
	if _, err := fmt.Fprintf(out, "%s: %s (generation %d)\n", result.Namespace, result.Phase, result.Generation); err != nil {
		return err
	}
	for _, url := range result.URLs {
		if _, err := fmt.Fprintf(out, "%s: %s\n", url.Service, url.URL); err != nil {
			return err
		}
	}
	return nil
}
