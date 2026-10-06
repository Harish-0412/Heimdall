package render

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const shopflowConfig = "../../examples/shopflow/heimdall.yaml"

// loadFile loads a config the way every production caller does.
func loadFile(t testing.TB, path string, policy config.Policy) *config.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, diags := config.Load(f, policy)
	if cfg == nil {
		t.Fatalf("%s is invalid: %+v", path, diags)
	}
	return cfg
}

func loadString(t testing.TB, src string) *config.Config {
	t.Helper()
	cfg, diags := config.Load(strings.NewReader(src), config.DefaultPolicy())
	if cfg == nil {
		t.Fatalf("invalid config: %+v\n%s", diags, src)
	}
	return cfg
}

// testCredentials are fixed so golden files are stable. Real ones come from
// GenerateCredentials.
var testCredentials = Credentials{
	PostgresSuperuser: "PostgresSuperuser0000000",
	PostgresApp:       "PostgresApp0000000000000",
	Redis:             "Redis0000000000000000000",
	RabbitMQ:          "RabbitMQ0000000000000000",
}

// testContext is a complete, valid context for cfg: every workload has a
// pinned image and a declared seed has content.
func testContext(t testing.TB, cfg *config.Config) Context {
	t.Helper()
	creds := testCredentials
	ctx := Context{
		Tenant:        "acme",
		Repo:          "acme/shopflow",
		PR:            184,
		SHA:           "4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39",
		Generation:    3,
		EnvironmentID: "env-0f8b2c",
		Owner:         "octocat",
		ExpiresAt:     time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		URLSuffix:     "x7d2",
		Images:        fakeImages(cfg),
		Credentials:   &creds,
		Policy:        config.DefaultPolicy(),
		Platform:      Platform{BaseDomain: "preview.example.com"},
	}
	if p := cfg.Dependencies.Postgres; p != nil && p.Seed != "" {
		ctx.Seed = []byte("SELECT current_database();\n")
	}
	return ctx
}

// fakeImages pins every workload not already pinned in the config to a
// deterministic digest, keeping the declared repository for `image:` ones.
func fakeImages(cfg *config.Config) map[string]string {
	images := map[string]string{}
	pin := func(name, declared string) {
		if strings.Contains(declared, "@sha256:") {
			return
		}
		sum := sha256.Sum256([]byte(name))
		digest := "@sha256:" + hex.EncodeToString(sum[:])
		if declared != "" {
			repo, _ := splitImage(declared)
			images[name] = repo + digest
			return
		}
		images[name] = "registry.example.com/acme/" + name + ":pr184" + digest
	}
	for name, s := range cfg.Services {
		pin(name, s.Image)
	}
	for name, w := range cfg.Workers {
		pin(name, w.Image)
	}
	return images
}

func mustRender(t testing.TB, cfg *config.Config, ctx Context) *Plan {
	t.Helper()
	plan, err := Render(cfg, ctx)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return plan
}

func shopflowPlan(t testing.TB) (*Plan, Context) {
	t.Helper()
	cfg := loadFile(t, shopflowConfig, config.DefaultPolicy())
	ctx := testContext(t, cfg)
	return mustRender(t, cfg, ctx), ctx
}

// find returns the object of the given kind and name, or nil.
func find[T Object](p *Plan, name string) T {
	for _, o := range p.Objects() {
		if t, ok := o.(T); ok && o.GetName() == name {
			return t
		}
	}
	var zero T
	return zero
}
