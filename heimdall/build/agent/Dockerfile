# syntax=docker/dockerfile:1
# The Heimdall agent image. Build from the repository root:
#   docker build -f build/agent/Dockerfile -t heimdall-agent .
#
# Base images are pinned by digest; Go is cross-compiled for the target
# platform, so multi-architecture builds (buildx) need no emulation.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=none
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/heimdall-dev/heimdall/internal/version.Version=${VERSION} -X github.com/heimdall-dev/heimdall/internal/version.Commit=${COMMIT}" \
      -o /out/agent ./cmd/agent

# Distroless: no shell, no package manager; runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]
