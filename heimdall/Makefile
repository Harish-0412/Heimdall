MODULE   := github.com/heimdall-dev/heimdall
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -X $(MODULE)/internal/version.Version=$(VERSION) \
            -X $(MODULE)/internal/version.Commit=$(COMMIT)

KUBE_VERSION  ?= 1.33.0
# Schemas for CRDs (HTTPRoute) that kubeconform does not ship.
CRDS_CATALOG  := https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json
RENDER_OUT    := out/render

.PHONY: build test cover lint fmt vet tidy validate-examples schema golden \
        render-examples kubeconform helm-lint fuzz e2e-kind ci

build:
	go build -ldflags "$(LDFLAGS)" -o bin/heimdall ./cmd/heimdall

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

helm-lint:
	helm lint --strict charts/heimdall-agent

fuzz:
	go test ./internal/render -run '^$$' -fuzz '^FuzzNames$$' -fuzztime 30s
	go test ./internal/render -run '^$$' -fuzz '^FuzzLabelValue$$' -fuzztime 30s

# The P1 exit criterion: ShopFlow reaches Ready on kind (needs Docker).
e2e-kind:
	bash test/e2e/kind/run.sh

ci: vet test validate-examples kubeconform helm-lint
