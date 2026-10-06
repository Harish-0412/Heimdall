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
	"path/filepath"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const renderUsage = `Usage: heimdall render [flags] [file]

Prints the Kubernetes objects Heimdall would apply for a preview of the given
heimdall.yaml (default: ./heimdall.yaml), stage by stage. Nothing touches a
cluster. In production the control plane supplies the context flags.

Examples:
  heimdall render --placeholder-images --stage guardrails
  heimdall render --image api=ghcr.io/acme/api@sha256:... --out-dir out/
  heimdall render --list --placeholder-images

Flags:
`

// renderFlags holds parsed flag values; context building happens after parsing
// so defaults can depend on the config file.
type renderFlags struct {
	tenant, repo, sha, envID, owner, expires, suffix string
	pr                                               int
	generation                                       int64
	images                                           imageFlag
	imagesFile                                       string
	placeholders, generateCredentials                bool
	repoRoot                                         string

	baseDomain, scheme, gateway, storageClass, imageMirror string
	urlPort                                                int
	egress                                                 listFlag
	nodeSelector                                           imageFlag // key=value

	stage, outDir string
	list          bool
}

func runRender(args []string, stdout, stderr io.Writer) int {
	var f renderFlags
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, renderUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.tenant, "tenant", "local", "tenant slug")
	fs.StringVar(&f.repo, "repo", "", `repository as owner/name (default "local/<config directory name>")`)
	fs.IntVar(&f.pr, "pr", 1, "pull request number")
	fs.StringVar(&f.sha, "sha", strings.Repeat("0", 40), "full commit SHA")
	fs.Int64Var(&f.generation, "generation", 1, "deployment generation")
	fs.StringVar(&f.envID, "env-id", "", "environment id (default <tenant>-<repo name>-pr<N>)")
	fs.StringVar(&f.owner, "owner", "local", "GitHub login of the PR author")
	fs.StringVar(&f.expires, "expires", "", "expiry as RFC 3339 (default now + preview.ttl)")
	fs.StringVar(&f.suffix, "suffix", "", "hostname suffix, 4-8 of [a-z0-9] (default derived from tenant, repo and PR; not secret)")
	fs.Var(&f.images, "image", "pinned image for a workload, name=ref@sha256:... (repeatable)")
	fs.StringVar(&f.imagesFile, "images", "", "JSON file mapping workload names to pinned images, as produced by CI")
	fs.BoolVar(&f.placeholders, "placeholder-images", false, "fill missing images with non-runnable placeholders (for inspection)")
	fs.BoolVar(&f.generateCredentials, "generate-credentials", false, "include the credentials Secret, with fresh random passwords")
	fs.StringVar(&f.repoRoot, "repo-root", "", "repository root the seed path is relative to (default: the config file's directory)")
	fs.StringVar(&f.baseDomain, "base-domain", "localtest.me", "preview DNS zone (*.localtest.me resolves to 127.0.0.1)")
	fs.StringVar(&f.scheme, "scheme", "http", "URL scheme of preview URLs: http or https")
	fs.IntVar(&f.urlPort, "url-port", 0, "port added to preview URLs (0: the scheme's default)")
	fs.StringVar(&f.gateway, "gateway", render.DefaultGatewayNamespace+"/"+render.DefaultGatewayName, "shared Gateway as namespace/name[/listener]")
	fs.StringVar(&f.storageClass, "storage-class", "", "storage class for the database volume (default: the cluster default)")
	fs.Var(&f.egress, "egress-cidr", "CIDR previews may reach outside the cluster (repeatable; default none)")
	fs.StringVar(&f.imageMirror, "image-mirror", "", "registry mirror for Heimdall's own images")
	fs.Var(&f.nodeSelector, "node-selector", "key=value label of the preview node pool (repeatable)")
	fs.StringVar(&f.stage, "stage", "", "print only this stage: "+stageList())
	fs.StringVar(&f.outDir, "out-dir", "", "write one numbered file per step into this directory instead of printing")
	fs.BoolVar(&f.list, "list", false, "print the namespace, URLs and steps instead of objects")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "heimdall: render takes at most one file")
		return ExitUsage
	}
	if f.stage != "" && !isStage(f.stage) {
		fmt.Fprintf(stderr, "heimdall: unknown --stage %q (want one of %s)\n", f.stage, stageList())
		return ExitUsage
	}
	file := defaultConfigFile
	if len(pos) == 1 {
		file = pos[0]
	}

	policy := config.DefaultPolicy()
	cfg, code := loadConfig(file, policy, stderr)
	if cfg == nil {
		return code
	}
	ctx, err := f.context(file, cfg, policy)
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: %v\n", err)
		return ExitUsage
	}

	plan, err := render.Render(cfg, ctx)
	if err != nil {
		var errs render.Errors
		if !errors.As(err, &errs) {
			fmt.Fprintf(stderr, "heimdall: %v\n", err)
			return ExitUsage
		}
		for _, e := range errs {
			fmt.Fprintf(stderr, "%s: error[%s]: %s: %s\n", file, e.Code, e.Field, e.Message)
		}
		fmt.Fprintf(stderr, "%s: cannot render (%s)\n", file, plural(len(errs), "error"))
		return ExitInvalid
	}
	if f.placeholders {
		fmt.Fprintln(stderr, "heimdall: note: placeholder images are not runnable; supply --image for a real deployment")
	}

	switch {
	case f.list:
		err = writeList(stdout, plan)
	case f.outDir != "":
		err = writeFiles(stdout, f.outDir, plan, render.StageName(f.stage))
	default:
		err = render.WritePlan(stdout, plan, render.StageName(f.stage))
	}
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}

// loadConfig loads and validates file, printing diagnostics on failure.
// Warnings are printed too but do not stop rendering.
func loadConfig(file string, policy config.Policy, stderr io.Writer) (*config.Config, int) {
	fh, err := os.Open(file)
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: cannot read config: %v\n", err)
		return nil, ExitUsage
	}
	defer fh.Close()
	cfg, diags := config.Load(fh, policy)
	writeText(stderr, file, diags)
	if cfg == nil {
		fmt.Fprintf(stderr, "%s: %s\n", file, counts(diags))
		return nil, ExitInvalid
	}
	return cfg, ExitOK
}

// context builds the render context from flags, with local-friendly defaults.
func (f *renderFlags) context(file string, cfg *config.Config, policy config.Policy) (render.Context, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return render.Context{}, err
	}
	configDir := filepath.Dir(abs)
	repo := f.repo
	if repo == "" {
		repo = "local/" + sanitizeName(filepath.Base(configDir))
	}
	_, repoName, _ := strings.Cut(repo, "/")
	envID := f.envID
	if envID == "" {
		envID = fmt.Sprintf("%s-%s-pr%d", f.tenant, sanitizeName(repoName), f.pr)
	}
	suffix := f.suffix
	if suffix == "" {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s#%d", f.tenant, repo, f.pr))
		suffix = hex.EncodeToString(sum[:2])
	}
	expires := time.Now().UTC().Add(cfg.Preview.TTL.Std()).Truncate(time.Second)
	if f.expires != "" {
		if expires, err = time.Parse(time.RFC3339, f.expires); err != nil {
			return render.Context{}, fmt.Errorf("--expires: %w", err)
		}
	}
	gw, err := parseGateway(f.gateway)
	if err != nil {
		return render.Context{}, err
	}

	images := map[string]string{}
	if f.imagesFile != "" {
		data, err := os.ReadFile(f.imagesFile)
		if err != nil {
			return render.Context{}, fmt.Errorf("--images: %w", err)
		}
		if err := json.Unmarshal(data, &images); err != nil {
			return render.Context{}, fmt.Errorf("--images %s: want a JSON object of name to image: %w", f.imagesFile, err)
		}
	}
	for k, v := range f.images {
		images[k] = v
	}
	if f.placeholders {
		fillPlaceholders(cfg, images)
	}

	ctx := render.Context{
		Tenant: f.tenant, Repo: repo, PR: f.pr, SHA: f.sha, Generation: f.generation,
		EnvironmentID: envID, Owner: f.owner, ExpiresAt: expires, URLSuffix: suffix,
		Images: images, Policy: policy,
		Platform: render.Platform{
			BaseDomain: f.baseDomain, URLScheme: f.scheme, URLPort: f.urlPort, Gateway: gw,
			StorageClassName: f.storageClass, EgressCIDRs: f.egress, ImageMirror: f.imageMirror,
			NodeSelector: f.nodeSelector,
		},
	}
	if f.generateCredentials {
		ctx.Credentials = render.GenerateCredentials()
	}
	if p := cfg.Dependencies.Postgres; p != nil && p.Seed != "" {
		root := f.repoRoot
		if root == "" {
			root = configDir
		}
		if ctx.Seed, err = readSeed(root, p.Seed); err != nil {
			return render.Context{}, err
		}
	}
	return ctx, nil
}

// readSeed reads a repository-relative file through os.Root, so a symlink in
// the repository cannot point the read outside it. The size is capped one byte
// past the limit, leaving the precise error to render.
func readSeed(root, name string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	defer r.Close()
	fh, err := r.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, render.MaxSeedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return data, nil
}

// fillPlaceholders gives every workload without an image a syntactically valid,
// deliberately unresolvable reference (the .invalid TLD never resolves).
func fillPlaceholders(cfg *config.Config, images map[string]string) {
	add := func(name, declared string) {
		if _, ok := images[name]; ok || strings.Contains(declared, "@sha256:") {
			return
		}
		sum := sha256.Sum256([]byte(name))
		images[name] = "placeholder.invalid/" + name + ":unbuilt@sha256:" + hex.EncodeToString(sum[:])
		if declared != "" {
			repo, _, _ := strings.Cut(declared, "@")
			if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
				repo = repo[:i]
			}
			images[name] = repo + "@sha256:" + hex.EncodeToString(sum[:])
		}
	}
	for name, s := range cfg.Services {
		add(name, s.Image)
	}
	for name, w := range cfg.Workers {
		add(name, w.Image)
	}
}

func writeList(w io.Writer, p *render.Plan) error {
	fmt.Fprintf(w, "namespace: %s\n", p.Namespace)
	for _, u := range p.URLs {
		primary := ""
		if u.Primary {
			primary = " (primary)"
		}
		fmt.Fprintf(w, "url: %s %s%s\n", u.Service, u.URL, primary)
	}
	files, err := render.Files(p)
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Fprintf(w, "step: %s\n", strings.TrimSuffix(f.Name, ".yaml"))
	}
	return nil
}

func writeFiles(stdout io.Writer, dir string, p *render.Plan, only render.StageName) error {
	files, err := render.Files(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if only != "" && f.Stage != only {
			continue
		}
		path := filepath.Join(dir, f.Name)
		// 0600: the guardrails file holds credentials when they are generated.
		if err := os.WriteFile(path, f.Data, 0o600); err != nil {
			return err
		}
		fmt.Fprintln(stdout, path)
	}
	return nil
}

func parseGateway(s string) (render.GatewayRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return render.GatewayRef{}, fmt.Errorf("--gateway %q: want namespace/name or namespace/name/listener", s)
	}
	ref := render.GatewayRef{Namespace: parts[0], Name: parts[1]}
	if len(parts) == 3 {
		ref.SectionName = parts[2]
	}
	return ref, nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "app"
	}
	return out
}

func isStage(s string) bool {
	for _, n := range render.StageNames() {
		if string(n) == s {
			return true
		}
	}
	return false
}

func stageList() string {
	var names []string
	for _, n := range render.StageNames() {
		names = append(names, string(n))
	}
	return strings.Join(names, ", ")
}

// imageFlag collects repeated --image name=ref values.
type imageFlag map[string]string

func (m *imageFlag) String() string { return fmt.Sprint(map[string]string(*m)) }

func (m *imageFlag) Set(v string) error {
	name, ref, ok := strings.Cut(v, "=")
	if !ok || name == "" || ref == "" {
		return fmt.Errorf("want name=image, got %q", v)
	}
	if *m == nil {
		*m = imageFlag{}
	}
	(*m)[name] = ref
	return nil
}

// listFlag collects repeated string values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}
