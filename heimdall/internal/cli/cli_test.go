package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const example = "../../examples/shopflow/heimdall.yaml"

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "heimdall.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate_ExampleIsValid(t *testing.T) {
	code, out, errs := run("validate", example)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, errs)
	}
	if !strings.Contains(out, "valid (2 services, 1 worker, dependencies: postgres, rabbitmq, redis)") {
		t.Errorf("unexpected summary: %q", out)
	}
}

func TestValidate_InvalidReportsLocationAndHint(t *testing.T) {
	p := writeTemp(t, "version: 1\nservices:\n  api:\n    build: {context: .}\n    port: 70000\n")
	code, _, errs := run("validate", p)
	if code != ExitInvalid {
		t.Fatalf("exit = %d, want %d", code, ExitInvalid)
	}
	for _, want := range []string{":5:", "error[port.invalid]", "1 error"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
}

func TestValidate_MissingFileIsUsageError(t *testing.T) {
	code, _, errs := run("validate", filepath.Join(t.TempDir(), "nope.yaml"))
	if code != ExitUsage || !strings.Contains(errs, "cannot read config") {
		t.Fatalf("exit = %d, stderr = %q", code, errs)
	}
}

func TestValidate_JSON(t *testing.T) {
	p := writeTemp(t, "version: 1\n")
	code, out, _ := run("validate", "--format", "json", p)
	if code != ExitInvalid {
		t.Fatalf("exit = %d", code)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if rep.Valid || rep.Errors != 1 || rep.Diagnostics[0].Code != "services.empty" {
		t.Errorf("unexpected report: %+v", rep)
	}
}

func TestValidate_JSONValidHasEmptyArray(t *testing.T) {
	code, out, _ := run("validate", "--format=json", example)
	if code != ExitOK || !strings.Contains(out, `"diagnostics": []`) {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
}

func TestValidate_StrictFailsOnWarnings(t *testing.T) {
	// No health check -> warning only.
	p := writeTemp(t, "version: 1\nservices:\n  api: {build: {context: .}, port: 80, public: true}\n")

	if code, _, errs := run("validate", p); code != ExitOK {
		t.Fatalf("non-strict exit = %d\n%s", code, errs)
	}
	// Flag after the positional argument must still be honoured.
	if code, _, _ := run("validate", p, "--strict"); code != ExitInvalid {
		t.Fatalf("strict exit = %d, want %d", code, ExitInvalid)
	}
}

func TestRun_UsageAndVersion(t *testing.T) {
	if code, _, _ := run(); code != ExitUsage {
		t.Errorf("no args exit = %d", code)
	}
	if code, _, errs := run("bogus"); code != ExitUsage || !strings.Contains(errs, "unknown command") {
		t.Errorf("unknown command exit = %d, %q", code, errs)
	}
	if code, out, _ := run("version"); code != ExitOK || !strings.HasPrefix(out, "heimdall ") {
		t.Errorf("version exit = %d, %q", code, out)
	}
	if code, out, _ := run("help"); code != ExitOK || !strings.Contains(out, "validate") {
		t.Errorf("help exit = %d", code)
	}
	if code, _, _ := run("validate", "--format", "xml", example); code != ExitUsage {
		t.Errorf("bad format exit = %d", code)
	}
}

func TestValidate_BaselineBlocksLoosening(t *testing.T) {
	const base = "version: 1\nservices:\n  api: {build: {context: .}, port: 80, public: true, health: {path: /h}}\npreview: {visibility: private}\n"
	const pr = "version: 1\nservices:\n  api: {build: {context: .}, port: 80, public: true, health: {path: /h}}\npreview: {visibility: org}\n"
	b, p := writeTemp(t, base), writeTemp(t, pr)

	code, _, errs := run("validate", "--baseline", b, p)
	if code != ExitInvalid || !strings.Contains(errs, "trust.visibility") || !strings.Contains(errs, ":4:") {
		t.Fatalf("exit = %d, stderr:\n%s", code, errs)
	}
	if code, _, errs := run("validate", "--baseline", b, b); code != ExitOK {
		t.Fatalf("identical config must pass, exit = %d\n%s", code, errs)
	}
	if code, _, errs := run("validate", "--baseline", filepath.Join(t.TempDir(), "x.yaml"), p); code != ExitUsage {
		t.Fatalf("missing baseline exit = %d\n%s", code, errs)
	}
}
