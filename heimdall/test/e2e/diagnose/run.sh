#!/usr/bin/env bash
# Phase 4 exit criteria (docs/phases.md): break ShopFlow on kind in each way
# the diagnostics engine must explain (one scenario per diagnosis code: bad
# image, bad migration, out of memory, quota, crashing app, failing smoke
# test, failing health check, rejected route, no capacity, database down,
# failed and unapproved imports, missing secret, stale generation, step
# timeout) and check the code and message each yields, through `heimdall up`
# and `heimdall diagnose`.
#
# HEIMDALL_E2E_CAPTURE=1 also refreshes the rule fixtures in
# internal/diagnose/testdata/scenarios from the captured snapshots;
# HEIMDALL_E2E_RUN selects scenarios (a go test -run pattern).
# Requirements: docker, kind, kubectl, go. KEEP_CLUSTER=1 keeps the cluster.
set -euo pipefail
export MSYS_NO_PATHCONV=1
PATH="$(go env GOPATH)/bin:$PATH"
native() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
ROOT=$(native "$(dirname "${BASH_SOURCE[0]}")/../../..")
CLUSTER=${CLUSTER:-heimdall-p4}
REGISTRY=${REGISTRY:-heimdall-p4-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5004}
GATEWAY_API_VERSION=v1.6.2
NGF_VERSION=v2.7.0
WORK="$ROOT/out/diagnose-e2e"
mkdir -p "$WORK"
cleanup() {
  if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

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
if [[ $(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "$REGISTRY") == null ]]; then
  docker network connect kind "$REGISTRY"
fi
k() { kubectl --context "kind-$CLUSTER" "$@"; }
# A real Gateway (as in the P1 suite), so a rejected route is judged by a
# real controller (scenario route-rejected).
k apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml" >/dev/null
k apply --server-side -f "https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/$NGF_VERSION/deploy/crds.yaml" >/dev/null
k apply -f "https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/$NGF_VERSION/deploy/nodeport/deploy.yaml" >/dev/null
k -n nginx-gateway rollout status deployment/nginx-gateway --timeout=300s >/dev/null
k apply -f "$ROOT/test/e2e/kind/gateway.yaml" >/dev/null
k -n heimdall-gateway wait --for=condition=Programmed gateway/heimdall --timeout=300s >/dev/null

push() {
  local ref="localhost:$REGISTRY_PORT/shopflow-$1:p4" digest
  docker build --quiet --tag "$ref" "$ROOT/examples/shopflow/$1" >/dev/null
  digest=$(docker push "$ref" | sed -n 's/.*digest: \(sha256:[a-f0-9]\{64\}\).*/\1/p' | tail -n1)
  [[ $digest == sha256:* ]] || { echo "no digest for $ref" >&2; exit 1; }
  echo "localhost:$REGISTRY_PORT/shopflow-$1@$digest"
}
API_IMAGE=$(push api)
WEB_IMAGE=$(push web)
printf '{"api":"%s","notifications":"%s","web":"%s"}\n' "$API_IMAGE" "$API_IMAGE" "$WEB_IMAGE" > "$WORK/images.json"

export HEIMDALL_E2E_CONTEXT="kind-$CLUSTER"
export HEIMDALL_E2E_IMAGES="$WORK/images.json"
export HEIMDALL_E2E_CLI="$WORK/heimdall$(go env GOEXE)"
export HEIMDALL_E2E_FIXTURES="$ROOT/internal/diagnose/testdata/scenarios"
cd "$ROOT"
go build -o "$HEIMDALL_E2E_CLI" ./cmd/heimdall
go test -v -count=1 -timeout 60m ${HEIMDALL_E2E_RUN:+-run "$HEIMDALL_E2E_RUN"} ./test/e2e/diagnose
