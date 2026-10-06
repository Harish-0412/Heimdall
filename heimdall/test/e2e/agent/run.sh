#!/usr/bin/env bash
# Phase 3 exit criteria (docs/phases.md) on kind, through the real agent
# image and chart:
#   - kubectl apply a PreviewEnvironment -> Ready; spec change -> rollout;
#     reset; delete -> fully gone;
#   - admission: invalid configs, forged status and stale generations are
#     rejected; N+1 cancels N;
#   - a failed migration is diagnosed in status (P4) and cleared by the fix;
#   - the leader is killed (process SIGKILL and pod deletion) in each stage of
#     an apply and during a destroy, and the environment still converges;
#   - a hand-labelled orphan is removed only after the grace period, and not
#     at all while the desired-state source is unreachable;
#   - the configmap source creates and removes environments;
#   - metrics require authentication.
#
# Requirements: docker, kind, kubectl, helm, go. Works in Git Bash on Windows.
# KEEP_CLUSTER=1 keeps the cluster afterwards.
# HEIMDALL_E2E_RUN selects Go test cases or subtests by regular expression.
set -euo pipefail
export MSYS_NO_PATHCONV=1
PATH="$(go env GOPATH)/bin:$PATH"
native() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
ROOT=$(native "$(dirname "${BASH_SOURCE[0]}")/../../..")
HERE="$ROOT/test/e2e/agent"
CLUSTER=${CLUSTER:-heimdall-p3}
REGISTRY=${REGISTRY:-heimdall-p3-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5003}
REGISTRY_IMAGE=public.ecr.aws/docker/library/registry:3
GATEWAY_API_VERSION=v1.6.2
AGENT_NS=heimdall-system
WORK="$ROOT/out/agent-e2e"
mkdir -p "$WORK"
k() { kubectl --context "kind-$CLUSTER" "$@"; }
log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

cleanup() {
  if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
for cmd in docker kind kubectl helm go; do
  command -v "$cmd" >/dev/null || { echo "$cmd is required" >&2; exit 1; }
done

log "Cluster and local registry"
if [[ $(docker inspect -f '{{.State.Running}}' "$REGISTRY" 2>/dev/null || true) != true ]]; then
  docker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" --name "$REGISTRY" "$REGISTRY_IMAGE" >/dev/null
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
# HTTPRoute CRDs only: route acceptance is not part of readiness, so no
# gateway controller is needed (P1's e2e covers serving through one).
k apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml" >/dev/null

log "Images: ShopFlow and the agent, pinned by digest"
push() { # name context [dockerfile] -> prints a digest-pinned reference
  local ref="localhost:$REGISTRY_PORT/$1:e2e" digest
  docker build --quiet --tag "$ref" ${3:+--file "$3"} "$2" >/dev/null
  digest=$(docker push "$ref" | sed -n 's/.*digest: \(sha256:[a-f0-9]\{64\}\).*/\1/p' | tail -n1)
  [[ $digest == sha256:* ]] || { echo "no digest for $ref" >&2; exit 1; }
  echo "localhost:$REGISTRY_PORT/$1@$digest"
}
API_IMAGE=$(push shopflow-api "$ROOT/examples/shopflow/api")
WEB_IMAGE=$(push shopflow-web "$ROOT/examples/shopflow/web")
AGENT_IMAGE=$(push heimdall-agent "$ROOT" "$ROOT/build/agent/Dockerfile")
printf '{"api":"%s","notifications":"%s","web":"%s"}\n' "$API_IMAGE" "$API_IMAGE" "$WEB_IMAGE" > "$WORK/images.json"
echo "agent: $AGENT_IMAGE"

log "Install the agent chart"
helm upgrade --install heimdall-agent "$ROOT/charts/heimdall-agent" --kube-context "kind-$CLUSTER" \
  --namespace "$AGENT_NS" --create-namespace --values "$HERE/values.yaml" \
  --set image.repository="${AGENT_IMAGE%@*}" --set image.digest="${AGENT_IMAGE#*@}" \
  --wait --timeout 5m
k -n "$AGENT_NS" get pods -o wide

log "Exit criteria"
export HEIMDALL_E2E_CONTEXT="kind-$CLUSTER"
export HEIMDALL_E2E_IMAGES="$WORK/images.json"
export HEIMDALL_E2E_CLI="$WORK/heimdall$(go env GOEXE)"
export HEIMDALL_E2E_CHART="$ROOT/charts/heimdall-agent"
export HEIMDALL_E2E_SHA=$( (cd "$ROOT" && git rev-parse HEAD 2>/dev/null) || printf '0%.0s' {1..40})
cd "$ROOT"
go build -o "$HEIMDALL_E2E_CLI" ./cmd/heimdall
go test -v -count=1 -timeout 60m ${HEIMDALL_E2E_RUN:+-run "$HEIMDALL_E2E_RUN"} ./test/e2e/agent
