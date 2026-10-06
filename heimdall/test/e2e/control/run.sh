#!/usr/bin/env bash
# P5 exit criterion: real API, restricted Postgres, real outbound kind agent.
# Mutations use only the API; Kubernetes reads verify the resulting resources.
set -euo pipefail
export MSYS_NO_PATHCONV=1
PATH="$(go env GOPATH)/bin:$PATH"
native() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
ROOT=$(native "$(dirname "${BASH_SOURCE[0]}")/../../..")
CLUSTER=${CLUSTER:-heimdall-p5}
REGISTRY=${REGISTRY:-heimdall-p5-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5005}
WORK="$ROOT/out/control-e2e"
mkdir -p "$WORK"
k() { kubectl --context "kind-$CLUSTER" "$@"; }
cleanup() {
  if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
for cmd in docker kind kubectl helm go; do command -v "$cmd" >/dev/null || { echo "$cmd is required" >&2; exit 1; }; done
printf 'Preparing the dedicated P5 cluster and customer registry\n'
if [[ $(docker inspect -f '{{.State.Running}}' "$REGISTRY" 2>/dev/null || true) != true ]]; then
  docker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" --name "$REGISTRY" public.ecr.aws/docker/library/registry:3 >/dev/null
fi
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$ROOT/test/e2e/kind/cluster.yaml" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --wait 180s
fi
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:$REGISTRY_PORT"
  printf '[host."http://%s:5000"]\n' "$REGISTRY" | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:$REGISTRY_PORT/hosts.toml"
done
if [[ $(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "$REGISTRY") == null ]]; then docker network connect kind "$REGISTRY"; fi
k apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.2/standard-install.yaml >/dev/null
push() {
  local ref="localhost:$REGISTRY_PORT/$1:e2e" digest
  docker build --quiet --tag "$ref" ${3:+--file "$3"} "$2" >/dev/null
  digest=$(docker push "$ref" | sed -n 's/.*digest: \(sha256:[a-f0-9]\{64\}\).*/\1/p' | tail -n1)
  [[ $digest == sha256:* ]] || { echo "no pinned digest for $ref" >&2; exit 1; }
  echo "localhost:$REGISTRY_PORT/$1@$digest"
}
API_IMAGE=$(push shopflow-api "$ROOT/examples/shopflow/api")
WEB_IMAGE=$(push shopflow-web "$ROOT/examples/shopflow/web")
printf 'Building the current outbound agent\n'
AGENT_IMAGE=$(push heimdall-agent "$ROOT" "$ROOT/build/agent/Dockerfile")
printf 'Running real API acceptance with agent %s\n' "$AGENT_IMAGE"
printf '{"api":"%s","notifications":"%s","web":"%s"}\n' "$API_IMAGE" "$API_IMAGE" "$WEB_IMAGE" > "$WORK/images.json"
export HEIMDALL_E2E_CONTEXT="kind-$CLUSTER"
export HEIMDALL_E2E_IMAGES="$WORK/images.json"
export HEIMDALL_E2E_AGENT_IMAGE="$AGENT_IMAGE"
export HEIMDALL_E2E_CHART="$ROOT/charts/heimdall-agent"
export HEIMDALL_E2E_REGISTRY="localhost:$REGISTRY_PORT"
export HEIMDALL_E2E_BUNDLE_REGISTRY="$(docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$REGISTRY"):5000"
export HEIMDALL_E2E_API_HOST=${HEIMDALL_E2E_API_HOST:-host.docker.internal}
export TESTCONTAINERS_RYUK_DISABLED=${TESTCONTAINERS_RYUK_DISABLED:-true}
cd "$ROOT"
go test -tags=integration -v -count=1 -timeout 35m ./test/e2e/control
