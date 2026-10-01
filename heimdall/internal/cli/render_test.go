package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const pinned = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fixedRender pins every input that would otherwise vary between runs.
var fixedRender = []string{"render", "--expires", "2026-10-03T12:00:00Z", "--repo", "acme/shopflow", "--pr", "184"}

func TestRender_ExampleWithPlaceholders(t *testing.T) {
	code, out, errs := run(append(fixedRender, "--placeholder-images", example)...)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, errs)
	}
	for _, want := range []string{"# stage: guardrails, step: setup", "kind: Namespace", "name: heimdall-pr184-shopflow-", "kind: HTTPRoute"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(out, "kind: Secret") {
		t.Error("credentials must only be rendered on request")
	}
	if !strings.Contains(errs, "placeholder images are not runnable") {
		t.Errorf("stderr should warn about placeholders: %q", errs)
	}
	// Same flags, same output: the CLI is as deterministic as the renderer.
	if _, again, _ := run(append(fixedRender, "--placeholder-images", example)...); again != out {
		t.Error("render output differs between runs")
	}
}

func TestRender_MissingImagesListEveryWorkload(t *testing.T) {
	code, _, errs := run(append(fixedRender, example)...)
	if code != ExitInvalid {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitInvalid, errs)
	}
	for _, want := range []string{"render.image.missing", "Context.Images.api", "Context.Images.web", "Context.Images.notifications", "3 errors"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
}

func TestRender_ImagesFromFlagsAndFile(t *testing.T) {
	images := filepath.Join(t.TempDir(), "images.json")
	data, _ := json.Marshal(map[string]string{"web": "ghcr.io/acme/web" + pinned, "notifications": "ghcr.io/acme/api" + pinned})
	if err := os.WriteFile(images, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(append(fixedRender, "--images", images, "--image", "api=ghcr.io/acme/api:v2"+pinned, "--stage", "application", example)...)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, errs)
	}
	for _, want := range []string{"image: ghcr.io/acme/web" + pinned, "image: ghcr.io/acme/api:v2" + pinned} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(out, "kind: Namespace") {
		t.Error("--stage application must not print guardrails")
	}
}

func TestRender_ListAndOutDir(t *testing.T) {
	code, out, _ := run(append(fixedRender, "--placeholder-images", "--list", example)...)
	if code != ExitOK || !strings.HasPrefix(out, "namespace: heimdall-pr184-shopflow-") ||
		!strings.Contains(out, "(primary)") || !strings.Contains(out, "step: 09-smoke-run") {
		t.Fatalf("exit = %d, list:\n%s", code, out)
	}

	dir := filepath.Join(t.TempDir(), "out")
	code, out, errs := run(append(fixedRender, "--placeholder-images", "--generate-credentials", "--out-dir", dir, example)...)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, errs)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 9 || entries[0].Name() != "01-guardrails-setup.yaml" {
		t.Fatalf("files = %v, %v", entries, err)
	}
	guard, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if !bytes.Contains(guard, []byte("kind: Secret")) || !bytes.Contains(guard, []byte("name: heimdall-credentials")) {
		t.Error("--generate-credentials must render the credentials Secret")
	}
	if strings.Count(out, "\n") != 9 {
		t.Errorf("stdout should list the written files:\n%s", out)
	}
}

func TestRender_UsageAndConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"unknown stage", []string{"render", "--stage", "deploy", example}, ExitUsage, "unknown --stage"},
		{"bad gateway", []string{"render", "--gateway", "just-a-name", "--placeholder-images", example}, ExitUsage, "--gateway"},
		{"bad expires", []string{"render", "--expires", "tomorrow", "--placeholder-images", example}, ExitUsage, "--expires"},
		{"bad image flag", []string{"render", "--image", "api", example}, ExitUsage, "want name=image"},
		{"two files", []string{"render", example, example}, ExitUsage, "at most one file"},
		{"missing file", []string{"render", filepath.Join(os.TempDir(), "nope", "heimdall.yaml")}, ExitUsage, "cannot read config"},
		{"bad context", []string{"render", "--placeholder-images", "--tenant", "Not Valid", example}, ExitInvalid, "Context.Tenant"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := run(tc.args...)
			if code != tc.code || !strings.Contains(errs, tc.want) {
				t.Fatalf("exit = %d (want %d), stderr:\n%s", code, tc.code, errs)
			}
		})
	}
	t.Run("invalid config shows diagnostics", func(t *testing.T) {
		p := writeTemp(t, "version: 1\nservices:\n  api: {build: {context: .}, port: 70000}\n")
		code, _, errs := run("render", "--placeholder-images", p)
		if code != ExitInvalid || !strings.Contains(errs, "error[port.invalid]") {
			t.Fatalf("exit = %d, stderr:\n%s", code, errs)
		}
	})
}

func TestRender_SeedIsReadInsideTheRepository(t *testing.T) {
	const cfg = "version: 1\nservices:\n  api: {build: {context: .}, port: 80, public: true, health: {path: /h}}\n" +
		"dependencies: {postgres: {seed: db/seed.sql}}\n"
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "heimdall.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "heimdall.yaml")

	if code, _, errs := run("render", "--placeholder-images", file); code != ExitUsage || !strings.Contains(errs, "seed") {
		t.Fatalf("a missing seed file is a usage error; exit = %d\n%s", code, errs)
	}

	outside := filepath.Join(t.TempDir(), "secret.sql")
	if err := os.WriteFile(outside, []byte("SELECT 'host file';"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "db", "seed.sql")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	code, out, errs := run("render", "--placeholder-images", "--stage", "baseline-db", file)
	if code == ExitOK || strings.Contains(out, "host file") {
		t.Fatalf("a seed symlink escaping the repository must be refused; exit = %d\n%s", code, errs)
	}
}

func TestSchemaCommand(t *testing.T) {
	code, out, _ := run("schema")
	want, err := config.JSONSchema()
	if err != nil || code != ExitOK || out != string(want) {
		t.Fatalf("exit = %d, err = %v, matches = %v", code, err, out == string(want))
	}
	if code, _, _ := run("schema", "extra"); code != ExitUsage {
		t.Errorf("schema with arguments: exit = %d", code)
	}
	if code, out, _ := run("schema", "--help"); code != ExitOK || !strings.Contains(out, "yaml-language-server") {
		t.Errorf("schema --help: exit = %d, %q", code, out)
	}
}
