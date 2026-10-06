package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlanValidatesSyntaxBeforeTenantAuthorization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heimdall.yaml")
	document := "version: 1\nservices:\n  web:\n    build: {context: .}\n    port: 3000\n    resources: {cpu: 4000m, memory: 8192Mi}\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if Run([]string{"build-plan", "--config", path}, &out, &errOut) != ExitOK || !strings.Contains(out.String(), `"name":"web"`) {
		t.Fatalf("valid custom-limit build syntax rejected: %s", errOut.String())
	}
}
