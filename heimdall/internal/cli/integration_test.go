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

func TestInitPinsReviewedWorkflowAndRefusesOverwrites(t *testing.T) {
	ref := "Harish-0412/Heimdall/.github/workflows/heimdall-preview.yml@" + strings.Repeat("a", 40)
	var out, errOut bytes.Buffer
	if code := Run([]string{"init", "--workflow", strings.Replace(ref, strings.Repeat("a", 40), "main", 1)}, &out, &errOut); code == ExitOK {
		t.Fatal("mutable trusted workflow accepted")
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"init", "--workflow", ref}, &out, &errOut); code != ExitOK {
		t.Fatal(errOut.String())
	}
	var files map[string]string
	if json.Unmarshal(out.Bytes(), &files) != nil {
		t.Fatal("invalid filemap")
	}
	if cfg, d := config.Load(strings.NewReader(files["heimdall.yaml"]), config.DefaultPolicy()); cfg == nil {
		t.Fatalf("invalid starter: %v", d)
	}
	if !strings.Contains(files[".github/workflows/heimdall.yml"], "head.repo.id == github.event.repository.id") {
		t.Fatal("fork guard absent")
	}
	dir := t.TempDir()
	if Run([]string{"init", "--workflow", ref, "--out-dir", dir}, &out, &errOut) != ExitOK {
		t.Fatal(errOut.String())
	}
	before, _ := os.ReadFile(filepath.Join(dir, "heimdall.yaml"))
	if Run([]string{"init", "--workflow", ref, "--out-dir", dir}, &out, &errOut) == ExitOK {
		t.Fatal("existing files overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "heimdall.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("existing config changed")
	}
}
