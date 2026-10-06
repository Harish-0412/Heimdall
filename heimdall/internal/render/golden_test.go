package render

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenScenarios cover ShopFlow, a minimal app and each optional feature.
// Review golden diffs like code: they are exactly what lands in a cluster.
var goldenScenarios = []struct {
	name   string
	config string
	seed   string // file with the seed content, when not the default stub
	edit   func(*Context)
}{
	{name: "shopflow", config: shopflowConfig},
	{name: "minimal", config: "testdata/configs/minimal.yaml"},
	{name: "postgres-only", config: "testdata/configs/postgres-only.yaml"},
	{name: "prebuilt-images", config: "testdata/configs/prebuilt-images.yaml"},
	{name: "internal-only", config: "testdata/configs/internal-only.yaml"},
	{name: "sizes-and-waves", config: "testdata/configs/sizes-and-waves.yaml"},
	{name: "platform-options", config: "testdata/configs/platform-options.yaml", edit: func(c *Context) {
		c.Credentials = nil
		c.Policy.MinMemoryRequestPercent = 60
		c.Platform = Platform{
			BaseDomain:       "preview.acme.dev",
			URLScheme:        "https",
			URLPort:          8443,
			Gateway:          GatewayRef{Namespace: "edge", Name: "previews", SectionName: "https"},
			EgressCIDRs:      []string{"0.0.0.0/0", "::/0", "10.20.0.0/16"},
			StorageClassName: "gp3-encrypted",
			NodeSelector:     map[string]string{"heimdall.dev/pool": "previews"},
			Tolerations: []corev1.Toleration{{
				Key: "heimdall.dev/preview", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			}},
			RuntimeClassName:       "gvisor",
			ImageMirror:            "mirror.acme.dev/hub",
			WritableRootFilesystem: true,
		}
	}},
}

func TestGolden(t *testing.T) {
	for _, sc := range goldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			// The config must be loaded with the scenario's policy: defaults
			// (memory requests) depend on it.
			probe := Context{Policy: config.DefaultPolicy()}
			if sc.edit != nil {
				sc.edit(&probe)
			}
			cfg := loadFile(t, sc.config, probe.Policy)
			ctx := testContext(t, cfg)
			if sc.seed != "" {
				seed, err := os.ReadFile(sc.seed)
				if err != nil {
					t.Fatal(err)
				}
				ctx.Seed = seed
			}
			if sc.edit != nil {
				sc.edit(&ctx)
			}
			got := renderGolden(t, mustRender(t, cfg, ctx))

			path := filepath.Join("testdata", "golden", sc.name+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run: go test ./internal/render -run Golden -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the rendered output; review and run: go test ./internal/render -run Golden -update\n%s",
					path, firstDiff(want, got))
			}
		})
	}
}

// renderGolden is the plan summary followed by every object.
func renderGolden(t *testing.T, p *Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# namespace: %s\n", p.Namespace)
	for _, u := range p.URLs {
		fmt.Fprintf(&buf, "# url: %s %s (primary: %v)\n", u.Service, u.URL, u.Primary)
	}
	if err := WritePlan(&buf, p, ""); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// firstDiff reports the first differing line, which is usually enough to see
// what changed; the full diff is one `git diff` away after -update.
func firstDiff(want, got []byte) string {
	w, g := bytes.Split(want, []byte("\n")), bytes.Split(got, []byte("\n"))
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl []byte
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if !bytes.Equal(wl, gl) {
			return fmt.Sprintf("line %d:\n  want: %s\n  got:  %s", i+1, wl, gl)
		}
	}
	return "(identical lines; trailing bytes differ)"
}
