package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controller"
	"github.com/heimdall-dev/heimdall/internal/render"
	"github.com/heimdall-dev/heimdall/internal/strictyaml"
)

var fixedManifest = []string{"manifest", "--expires", "2026-10-03T12:00:00Z", "--repo", "acme/shopflow", "--pr", "184",
	"--tenant", "acme", "--owner", "octocat", "--sha", strings.Repeat("a", 40), "--generation", "3", "--reset-nonce", "2",
	"--image", "api=ghcr.io/acme/api" + pinned, "--image", "notifications=ghcr.io/acme/api" + pinned, "--image", "web=ghcr.io/acme/web" + pinned}

// The object the CLI prints is exactly what the agent admits and deploys.
func TestManifest_IsAdmittedByTheAgent(t *testing.T) {
	code, out, errs := run(append(fixedManifest, example)...)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, errs)
	}
	var pe v1alpha1.PreviewEnvironment
	if err := strictyaml.Unmarshal([]byte(out), &pe); err != nil {
		t.Fatalf("output is not a strict PreviewEnvironment: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	s := pe.Spec
	switch {
	case pe.Kind != "PreviewEnvironment" || pe.APIVersion != "heimdall.dev/v1alpha1" || pe.Namespace != "heimdall-system":
		t.Errorf("type or namespace: %+v", pe.TypeMeta)
	case pe.Name != "acme-shopflow-pr184" || s.EnvironmentID != pe.Name:
		t.Errorf("name %q, environmentID %q", pe.Name, s.EnvironmentID)
	case s.Config.Inline != string(raw) || s.Config.SHA256 != hex.EncodeToString(sum[:]):
		t.Error("config must be embedded verbatim with its digest")
	case s.Generation != 3 || s.ResetNonce != 2 || s.PullRequest != 184 || s.Commit != strings.Repeat("a", 40):
		t.Errorf("identity: %+v", s)
	case s.DesiredState != v1alpha1.DesiredRunning || len(s.Images) != 3 || s.Data != nil:
		t.Errorf("state, images or data: %+v", s)
	case s.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-03T12:00:00Z":
		t.Errorf("expiresAt %v", s.ExpiresAt)
	}
	if strings.Contains(out, "status:") || strings.Contains(out, "creationTimestamp") {
		t.Errorf("output must hold only what a user applies:\n%s", out)
	}
	specs := controller.SpecBuilder{Policy: config.DefaultPolicy(), Platform: render.Platform{BaseDomain: "preview.example.com"}}
	if _, err := specs.Validate(context.Background(), &pe); err != nil {
		t.Fatalf("the agent would reject it: %v", err)
	}
	if _, again, _ := run(append(fixedManifest, example)...); again != out {
		t.Error("manifest output differs between runs")
	}
}

func TestManifest_OptionsAndErrors(t *testing.T) {
	code, out, errs := run(append(fixedManifest, "--name", "pr-184", "--namespace", "previews", "--destroyed", "--format", "json", example)...)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, errs)
	}
	for _, want := range []string{`"name": "pr-184"`, `"namespace": "previews"`, `"desiredState": "Destroyed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s", want)
		}
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"manifest", "--repo", "acme/shopflow", "--sha", strings.Repeat("a", 40), example}, ExitInvalid, "render.image.missing"},
		{append(fixedManifest, "--data-configmap", "x", example), ExitInvalid, "need an import declared"},
		{append(fixedManifest, "--format", "toml", example), ExitUsage, "Usage: heimdall manifest"},
		{append(fixedManifest, "missing.yaml"), ExitUsage, "missing.yaml"},
	} {
		code, _, errs := run(tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: exit %d (want %d), stderr %q (want %q)", tc.args[len(tc.args)-3:], code, tc.code, errs, tc.want)
		}
	}
}
