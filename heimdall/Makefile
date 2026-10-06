MODULE   := github.com/heimdall-dev/heimdall
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -X $(MODULE)/internal/version.Version=$(VERSION) \
            -X $(MODULE)/internal/version.Commit=$(COMMIT)

KUBE_VERSION  ?= 1.33.0
CONTROLLER_GEN_VERSION ?= v0.22.0
ENVTEST_VERSION ?= v0.25.2
ENVTEST_K8S_VERSION ?= 1.37.0
ENVTEST_DIR ?= $(CURDIR)/out/envtest
ENVTEST_PKGS ?= ./internal/controller ./internal/api/... ./internal/source
AGENT_IMAGE ?= heimdall-agent:dev
# Schemas for CRDs (HTTPRoute) that kubeconform does not ship.
CRDS_CATALOG  := https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json
RENDER_OUT    := out/render

.PHONY: build test cover lint fmt vet tidy validate-examples schema golden \
        render-examples kubeconform helm-lint fuzz e2e-kind e2e-engine ci \
        build-agent docker-agent generate envtest e2e-agent e2e-diagnose diagnostics-golden

build:
	go build -ldflags "$(LDFLAGS)" -o bin/heimdall ./cmd/heimdall

build-agent:
	go build -ldflags "$(LDFLAGS)" -o bin/heimdall-agent ./cmd/agent

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

vet:
	go vet ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run

tidy:
	go mod tidy

validate-examples: build
	./bin/heimdall validate --strict examples/shopflow/heimdall.yaml

# Regenerate generated files. Review the diff before committing.
schema:
	go test ./internal/config -run Schema -update

golden:
	go test ./internal/render -run Golden -update

diagnostics-golden:
	go test ./internal/diagnose -run '^TestCapturedScenarios$$' -update

# PreviewEnvironment deep copies and CRD (charts/heimdall-agent/crds).
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
	  object paths=./internal/api/v1alpha1 \
	  crd:crdVersions=v1 output:crd:artifacts:config=charts/heimdall-agent/crds

# Controller, CRD validation and syncer tests against a real API server and
# etcd. Linux and macOS; on Windows run it in a container (docs/agent.md).
envtest:
	assets="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) \
	  use $(ENVTEST_K8S_VERSION) --bin-dir "$(ENVTEST_DIR)" -p path)" && \
	  test -n "$$assets" && KUBEBUILDER_ASSETS="$$assets" \
	  go test -race -count=1 $(ENVTEST_PKGS)

# Render every example and test scenario, one file per step.
render-examples: build
	rm -rf $(RENDER_OUT)
	./bin/heimdall render --placeholder-images --generate-credentials --out-dir $(RENDER_OUT)/shopflow examples/shopflow/heimdall.yaml
	for f in internal/render/testdata/configs/*.yaml; do \
	  ./bin/heimdall render --placeholder-images --generate-credentials \
	    --repo-root internal/render/testdata/configs \
	    --out-dir $(RENDER_OUT)/$$(basename $$f .yaml) $$f || exit 1; \
	done

# Validate rendered objects and the agent chart against Kubernetes schemas.
kubeconform: render-examples
	kubeconform -strict -summary -kubernetes-version $(KUBE_VERSION) \
	  -schema-location default -schema-location '$(CRDS_CATALOG)' $(RENDER_OUT)
	helm template heimdall-agent charts/heimdall-agent --namespace heimdall-system --kube-version $(KUBE_VERSION) | \
	  kubeconform -strict -summary -kubernetes-version $(KUBE_VERSION) -schema-location default -

# Lint the chart, and check that the configuration it renders for each source
# type is accepted by the agent's own (strict) parser.
helm-lint:
	helm lint --strict charts/heimdall-agent --set platform.baseDomain=preview.example.com
	mkdir -p $(RENDER_OUT)
	for source in cluster configmap; do \
	  helm template heimdall-agent charts/heimdall-agent --namespace heimdall-system --kube-version $(KUBE_VERSION) \
	    --set platform.baseDomain=preview.example.com --set source.type=$$source --show-only templates/configmap.yaml | \
	    sed -n '/^  agent.yaml: |/,$$p' | sed '1d; s/^    //' > $(RENDER_OUT)/agent-$$source.yaml && \
	  go run ./cmd/agent --check-config --config $(RENDER_OUT)/agent-$$source.yaml || exit 1; \
	done

docker-agent:
	docker build -f build/agent/Dockerfile -t $(AGENT_IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

fuzz:
	go test ./internal/render -run '^$$' -fuzz '^FuzzNames$$' -fuzztime 30s
	go test ./internal/render -run '^$$' -fuzz '^FuzzLabelValue$$' -fuzztime 30s

# The P1 exit criterion: ShopFlow reaches Ready on kind (needs Docker).
e2e-kind:
	bash test/e2e/kind/run.sh

# Phase 2: actual Kubernetes API and two schema-only ShopFlow environments.
e2e-engine:
	bash test/e2e/engine/run.sh

# Phase 3 exit criteria: the agent on kind, from kubectl apply to cleanup,
# including agent kills mid-operation and the sweeper (docs/agent.md).
e2e-agent:
	bash test/e2e/agent/run.sh

# Phase 4 exit criteria: all fifteen captured failures on kind, each diagnosed.
# HEIMDALL_E2E_CAPTURE=1 refreshes internal/diagnose/testdata/scenarios.
e2e-diagnose:
	bash test/e2e/diagnose/run.sh

ci: vet test envtest validate-examples kubeconform helm-lint
