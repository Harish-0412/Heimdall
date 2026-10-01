package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite golden files")

const committedSchema = "../../schema/heimdall.schema.json"

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaID, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(SchemaID)
	if err != nil {
		t.Fatalf("generated schema does not compile: %v", err)
	}
	return s
}

// yamlToJSONValue decodes YAML the way editors do before applying a schema.
func yamlToJSONValue(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSchema_CommittedFileIsCurrent(t *testing.T) {
	got, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(committedSchema, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(committedSchema)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/config -run Schema -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("schema/heimdall.schema.json is stale; run: go test ./internal/config -run Schema -update")
	}
}

func TestSchema_EveryFieldAndTypeIsDocumented(t *testing.T) {
	for name, typ := range knownTypes {
		if typeDocs[name] == "" && name != "Config" {
			t.Errorf("type %s has no description in typeDocs", name)
		}
		for _, f := range yamlFields(typ) {
			if fieldDocs[name+"."+f] == "" {
				t.Errorf("field %s.%s has no description in fieldDocs", name, f)
			}
		}
	}
	for key := range fieldSchemas {
		if _, ok := fieldDocs[key]; !ok {
			t.Errorf("fieldSchemas refers to unknown field %s", key)
		}
	}
}

func TestSchema_AcceptsEveryValidConfig(t *testing.T) {
	s := compileSchema(t)
	example, err := os.ReadFile("../../examples/shopflow/heimdall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]string{
		"shopflow": string(example),
		"minimal":  minimalValid,
		"baseline": baselineSrc,
		"everything": `
version: 1
services:
  web: {image: "ghcr.io/acme/web:1.2", port: 3000, public: true, primary: true, health: {path: /, initialDelay: 10s}}
  api:
    build: {context: api, dockerfile: Dockerfile.dev, args: {NODE_VERSION: 22, DEBUG: true}}
    port: 8080
    public: true
    env: {PORT_ADMIN: 9090, FEATURE_X: "on"}
    secrets: [STRIPE_TEST_KEY]
    resources: {cpu: "0.5", memory: 1Gi, requests: {cpu: 100m, memory: 512Mi}}
    dependsOn: [postgres, redis, rabbitmq]
workers:
  mailer: {build: {context: .}, command: run, replicas: 3, resources: {size: small}}
  quiet: {image: busybox, command: sleep 1, replicas: 0}
dependencies:
  postgres: {version: 17, storage: 2Gi, seed: fixtures/Dev.SQL}
  redis: {version: "6"}
  rabbitmq: {version: 3.12}
migrations: {service: api, command: npm run migrate, timeout: 20m}
smokeTests:
  - {name: health, command: curl -f http://api:8080/health, timeout: 90s}
preview: {ttl: 2d, sleepAfter: 1h30m, visibility: org}
`,
	}
	for name, src := range valid {
		t.Run(name, func(t *testing.T) {
			if cfg, diags := Load(strings.NewReader(src), DefaultPolicy()); cfg == nil {
				t.Fatalf("fixture must be valid for Load: %+v", diags)
			}
			if err := s.Validate(yamlToJSONValue(t, src)); err != nil {
				t.Errorf("schema rejects a config that Load accepts:\n%v", err)
			}
		})
	}
}

func TestSchema_RejectsCommonMistakes(t *testing.T) {
	s := compileSchema(t)
	invalid := map[string]string{
		"unknown field":      svc("    dependson: [x]\n"),
		"bad visibility":     minimalValid + "preview: {visibility: everyone}\n",
		"port out of range":  strings.Replace(minimalValid, "8080", "70000", 1),
		"build and image":    svc("    image: nginx\n"),
		"size with cpu":      svc("    resources: {size: small, cpu: 1}\n"),
		"unknown size":       svc("    resources: {size: huge}\n"),
		"memory unit":        svc("    resources: {memory: 512M}\n"),
		"null dependency":    minimalValid + "dependencies:\n  postgres:\n",
		"bad version":        minimalValid + "dependencies: {postgres: {version: 9}}\n",
		"wrong schema":       strings.Replace(minimalValid, "version: 1", "version: 2", 1),
		"no services":        "version: 1\n",
		"bad service name":   strings.Replace(minimalValid, "api:", "API:", 1),
		"bad duration":       minimalValid + "preview: {ttl: soon}\n",
		"lowercase secret":   svc("    secrets: [stripe]\n"),
		"worker w/o command": svc("workers:\n  w: {build: {context: .}}\n"),
		"seed not sql":       minimalValid + "dependencies: {postgres: {seed: dump.txt}}\n",
	}
	for name, src := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := s.Validate(yamlToJSONValue(t, src)); err == nil {
				t.Errorf("schema accepts an invalid config:\n%s", src)
			}
		})
	}
}
