#!/usr/bin/env bash
# Real API, worker, PostgreSQL, Redis and RabbitMQ. No seed or generated records.
set -euo pipefail
export MSYS_NO_PATHCONV=1
PATH="$(go env GOPATH)/bin:$PATH"
native() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
ROOT=$(native "$(dirname "${BASH_SOURCE[0]}")/../../..")
CLUSTER=${CLUSTER:-heimdall-p2}
REGISTRY=${REGISTRY:-heimdall-p2-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5002}
WORK="$ROOT/out/engine-e2e"
mkdir -p "$WORK"
cleanup() {
  if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$CLUSTER"
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$ROOT/test/e2e/kind/cluster.yaml" --wait 180s
fi
if ! docker inspect "$REGISTRY" >/dev/null 2>&1; then
  docker run -d -p "127.0.0.1:$REGISTRY_PORT:5000" --name "$REGISTRY" public.ecr.aws/docker/library/registry:3
fi
docker network connect kind "$REGISTRY" 2>/dev/null || true
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:$REGISTRY_PORT"
  printf '[host."http://%s:5000"]\n' "$REGISTRY" | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:$REGISTRY_PORT/hosts.toml"
done
push() {
  local ref="localhost:$REGISTRY_PORT/shopflow-$1:p2" digest
  docker build --quiet --tag "$ref" "$ROOT/examples/shopflow/$1" >/dev/null
  digest=$(docker push "$ref" | sed -n 's/.*digest: \(sha256:[a-f0-9]\{64\}\).*/\1/p' | tail -n1)
  [[ -n $digest ]]
  echo "localhost:$REGISTRY_PORT/shopflow-$1@$digest"
}
API_IMAGE=$(push api)
WEB_IMAGE=$(push web)
printf '{"api":"%s","notifications":"%s","web":"%s"}\n' "$API_IMAGE" "$API_IMAGE" "$WEB_IMAGE" > "$WORK/images.json"
export HEIMDALL_E2E_CONTEXT="kind-$CLUSTER"
export HEIMDALL_E2E_IMAGES="$WORK/images.json"
export HEIMDALL_E2E_CLI="$WORK/heimdall$(go env GOEXE)"
cd "$ROOT"
go build -o "$HEIMDALL_E2E_CLI" ./cmd/heimdall
go test -v -timeout 25m ./internal/engine ./internal/cli ./test/e2e/engine
