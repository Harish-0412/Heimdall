package diagnosee2e

import (
	"os"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// Every scenario breaks the preview at run time, not its configuration:
// each variant must still be a valid heimdall.yaml. Runs without a cluster.
func TestScenarioConfigsAreValid(t *testing.T) {
	base, err := os.ReadFile("../agent/heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range scenarios {
		cfg := string(base)
		if sc.config != nil {
			cfg = sc.config(cfg)
		}
		if c, diags := config.Load(strings.NewReader(cfg), config.DefaultPolicy()); c == nil || diags.Errors() > 0 {
			t.Errorf("%s: %v", sc.name, diags)
		}
	}
}
