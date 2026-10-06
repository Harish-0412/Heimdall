package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/heimdall-dev/heimdall/internal/bundle"
	"github.com/heimdall-dev/heimdall/internal/config"
)

var pinnedWorkflow = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/\.github/workflows/[A-Za-z0-9._-]+\.ya?ml@[a-f0-9]{40}$`)

func runInit(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	workflow := fs.String("workflow", "", "trusted reusable workflow owner/repo/.github/workflows/file.yml@40-character SHA")
	dir := fs.String("out-dir", "", "write new files here (default prints a JSON file map)")
	port := fs.Int("port", 3000, "application port for the starter config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() != 0 || !pinnedWorkflow.MatchString(*workflow) || *port < 1 || *port > 65535 {
		fmt.Fprintln(errOut, "heimdall: init requires --workflow pinned to a reviewed commit SHA and a valid --port")
		return ExitUsage
	}
	files := map[string]string{
		"heimdall.yaml":                  fmt.Sprintf("version: 1\nservices:\n  web:\n    build: {context: ., dockerfile: Dockerfile}\n    port: %d\n    public: true\npreview:\n  ttl: 4h\n  visibility: private\n", *port),
		".github/workflows/heimdall.yml": fmt.Sprintf("name: Heimdall Preview\non:\n  pull_request:\n    types: [opened, synchronize, reopened]\npermissions:\n  contents: read\n  id-token: write\nconcurrency:\n  group: heimdall-${{ github.event.pull_request.number }}\n  cancel-in-progress: true\njobs:\n  preview:\n    if: github.event.pull_request.head.repo.id == github.event.repository.id\n    uses: %s\n    with:\n      aws-role: ${{ vars.HEIMDALL_ROLE_ARN }}\n      aws-region: ${{ vars.HEIMDALL_AWS_REGION }}\n      registry: ${{ vars.HEIMDALL_REGISTRY }}\n      api-url: ${{ vars.HEIMDALL_API_URL }}\n", *workflow),
		".heimdall/SETUP.md":             "Review heimdall.yaml and your Dockerfile, then commit these files. Install your own Heimdall GitHub App on this repository and register it with your tenant/cluster. Configure HEIMDALL_ROLE_ARN, HEIMDALL_AWS_REGION, HEIMDALL_REGISTRY and HEIMDALL_API_URL repository variables. Restrict the AWS OIDC trust subject to this repository's pull requests AND the exact reusable workflow; the role may only push to this repository's preview registry prefix. The agent's independent IRSA role may only pull from that prefix. Never give PR jobs production roles or secrets. Fork PRs are disabled. See Heimdall docs/github-app-setup.md for the exact provisioning and acceptance steps. Private URL enforcement arrives in P7; keep previews on trusted networks until it passes.\n",
	}
	if *dir == "" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if enc.Encode(files) != nil {
			return ExitUsage
		}
		return ExitOK
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return ExitUsage
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Refuse before any writes if a file would be overwritten.
	for _, name := range keys {
		target := filepath.Join(root, filepath.FromSlash(name))
		if _, err := os.Lstat(target); err == nil || !os.IsNotExist(err) {
			fmt.Fprintln(errOut, "heimdall: init refuses to overwrite existing files")
			return ExitUsage
		}
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return ExitUsage
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return ExitUsage
	}
	defer r.Close()
	for _, name := range keys {
		if err = r.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0755); err != nil {
			fmt.Fprintln(errOut, "heimdall: cannot create setup directory")
			return ExitUsage
		}
		f, err := r.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return ExitUsage
		}
		_, err = f.WriteString(files[name])
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return ExitUsage
		}
	}
	fmt.Fprintf(out, "Prepared %d files in %s. Review and commit them.\n", len(files), root)
	return ExitOK
}

func runBundle(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "Usage: heimdall bundle pack|push [flags]")
		return ExitUsage
	}
	fs := flag.NewFlagSet("bundle "+args[0], flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("repo-root", ".", "repository root")
	file := fs.String("config", "heimdall.yaml", "repository-relative config")
	layout := fs.String("layout", ".heimdall/bundle", "OCI layout directory")
	ref := fs.String("reference", "", "customer registry/repository:tag to push")
	plain := fs.Bool("plain-http", false, "explicit local registry test mode")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() != 0 {
		return ExitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var value string
	var err error
	switch args[0] {
	case "pack":
		d, e := bundle.Pack(ctx, *root, *file, *layout)
		err = e
		value = d.Digest.String()
	case "push":
		value, err = bundle.Push(ctx, *layout, *ref, bundle.RegistryOptions{PlainHTTP: *plain})
	default:
		return ExitUsage
	}
	if err != nil {
		fmt.Fprintf(errOut, "heimdall: bundle operation failed: %v\n", err)
		return ExitInvalid
	}
	fmt.Fprintln(out, value)
	return ExitOK
}

// BuildPlan is JSON data for the trusted workflow, never shell fragments.
type buildItem struct {
	Name  string        `json:"name"`
	Build *config.Build `json:"build,omitempty"`
	Image string        `json:"image,omitempty"`
}

func runBuildPlan(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("build-plan", flag.ContinueOnError)
	fs.SetOutput(errOut)
	file := fs.String("config", "heimdall.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() != 0 {
		return ExitUsage
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return ExitUsage
	}
	cfg, d := config.Load(bytes.NewReader(b), config.BaselinePolicy())
	if cfg == nil {
		writeText(errOut, *file, d)
		return ExitInvalid
	}
	items := []buildItem{}
	for n, s := range cfg.Services {
		items = append(items, buildItem{Name: n, Build: s.Build, Image: s.Image})
	}
	for n, w := range cfg.Workers {
		items = append(items, buildItem{Name: n, Build: w.Build, Image: w.Image})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	if json.NewEncoder(out).Encode(items) != nil {
		return ExitUsage
	}
	return ExitOK
}
