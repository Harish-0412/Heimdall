#!/usr/bin/env bash
# Phase 1 exit criterion (docs/phases.md): applying `heimdall render` output,
# stage by stage, to a kind cluster brings ShopFlow to Ready.
#
# The script:
#   1. creates a kind cluster, a local registry and a Gateway API gateway;
#   2. builds the ShopFlow images and pushes them, so they are pinned by digest;
#   3. renders the preview and applies it step by step with server-side apply,
#      waiting for each step the way the engine (P2) will;
#   4. checks the schema-only preview through the gateway and live dependencies;
#   5. checks the security guarantees on the live cluster (no token, non-root,
#      read-only root, Pod Security, quota, network isolation);
#   6. installs the agent chart and checks its permissions (agent-rbac.sh);
#   7. checks that re-applying is a no-op and that rendering is deterministic.
#
# Requirements: docker, kind, kubectl, helm, go, curl. Works in Git Bash on Windows.
# Environment: KEEP_CLUSTER=1 keeps the cluster afterwards; KIND_NODE_IMAGE
# overrides kind's default node image.
set -euo pipefail

export MSYS_NO_PATHCONV=1 # Git Bash: do not rewrite /paths passed to kubectl exec
PATH="$(go env GOPATH)/bin:$PATH"

# Native paths (C:/... in Git Bash), since path conversion is off.
native() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
ROOT=$(native "$(dirname "${BASH_SOURCE[0]}")/../../..")
HERE="$ROOT/test/e2e/kind"
CLUSTER=${CLUSTER:-heimdall-e2e}
REGISTRY=${REGISTRY:-heimdall-e2e-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5001}
GATEWAY_API_VERSION=v1.6.2
NGF_VERSION=v2.7.0
REGISTRY_IMAGE=public.ecr.aws/docker/library/registry:3
PROBE_IMAGE=quay.io/curl/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777
TIMEOUT=${TIMEOUT:-300}
WORK=$(native "$(mktemp -d)")
k() { kubectl --context "kind-$CLUSTER" "$@"; }
NS=""
PF_PID=""
declare -a TIMINGS=()

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '    \033[32mok\033[0m %s\n' "$*"; }
count() { echo $#; }

dump() {
  [[ -n $NS ]] || return 0
  echo "---- diagnostics for $NS ----" >&2
  k -n "$NS" get all,pvc,events --sort-by=.metadata.creationTimestamp >&2 || true
  for pod in $(k -n "$NS" get pods -o name 2>/dev/null); do
    echo "---- $pod ----" >&2
    k -n "$NS" describe "$pod" | tail -n 25 >&2 || true
    k -n "$NS" logs "$pod" --all-containers --tail=40 >&2 || true
  done
}

fail() {
  printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2
  dump
  exit 1
}

cleanup() {
  [[ -n $PF_PID ]] && kill "$PF_PID" 2>/dev/null || true
  if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  else
    echo "Cluster kept: kubectl --context kind-$CLUSTER -n $NS get all"
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

for cmd in docker kind kubectl helm go curl; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

# ---------------------------------------------------------------------------
log "Cluster, local registry and gateway"

if [[ $(docker inspect -f '{{.State.Running}}' "$REGISTRY" 2>/dev/null || true) != true ]]; then
  docker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" --network bridge --name "$REGISTRY" "$REGISTRY_IMAGE" >/dev/null
fi
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$HERE/cluster.yaml" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --wait 180s
fi
# localhost:<port> inside the nodes must reach the registry container.
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:$REGISTRY_PORT"
  printf '[host."http://%s:5000"]\n' "$REGISTRY" | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:$REGISTRY_PORT/hosts.toml"
done
if [[ $(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "$REGISTRY") == null ]]; then
  docker network connect kind "$REGISTRY"
fi
ok "cluster kind-$CLUSTER with registry localhost:$REGISTRY_PORT"

k apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml" >/dev/null
k apply --server-side -f "https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/$NGF_VERSION/deploy/crds.yaml" >/dev/null
k apply -f "https://raw.githubusercontent.com/nginx/nginx-gateway-fabric/$NGF_VERSION/deploy/nodeport/deploy.yaml" >/dev/null
k -n nginx-gateway rollout status deployment/nginx-gateway --timeout="${TIMEOUT}s" >/dev/null
k apply -f "$HERE/gateway.yaml" >/dev/null
k -n heimdall-gateway wait --for=condition=Programmed gateway/heimdall --timeout="${TIMEOUT}s" >/dev/null ||
  fail "gateway not programmed"
ok "Gateway API $GATEWAY_API_VERSION with NGINX Gateway Fabric $NGF_VERSION"

# ---------------------------------------------------------------------------
log "Build heimdall and the ShopFlow images"

HEIMDALL="$WORK/heimdall$(go env GOEXE)"
(cd "$ROOT" && go build -o "$HEIMDALL" ./cmd/heimdall)
push() { # name context -> prints a digest-pinned reference
  local ref="localhost:$REGISTRY_PORT/shopflow-$1:e2e" digest
  docker build --quiet --tag "$ref" "$2" >/dev/null
  digest=$(docker push "$ref" | sed -n 's/.*digest: \(sha256:[a-f0-9]\{64\}\).*/\1/p' | tail -n1)
  [[ $digest == sha256:* ]] || fail "no digest for $ref"
  echo "localhost:$REGISTRY_PORT/shopflow-$1@$digest"
}
API_IMAGE=$(push api "$ROOT/examples/shopflow/api")
WEB_IMAGE=$(push web "$ROOT/examples/shopflow/web")
ok "api: $API_IMAGE"
ok "web: $WEB_IMAGE"

# ---------------------------------------------------------------------------
log "Render ShopFlow"

EXPIRES=$(date -u -d '+48 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo 2030-01-01T00:00:00Z)
RENDER=("$HEIMDALL" render
  --tenant e2e --repo acme/shopflow --pr 184 --generation 1 --owner e2e
  --sha "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf '%040d' 0)"
  --expires "$EXPIRES" --base-domain localtest.me --scheme http
  --image "api=$API_IMAGE" --image "web=$WEB_IMAGE" --image "notifications=$API_IMAGE")
CONFIG="$ROOT/examples/shopflow/heimdall.yaml"

"${RENDER[@]}" --list "$CONFIG" >"$WORK/list"
NS=$(awk '/^namespace:/ {print $2}' "$WORK/list")
API_HOST=$(awk '/^url: api / {sub("http://", "", $3); print $3}' "$WORK/list")
WEB_HOST=$(awk '/^url: web / {sub("http://", "", $3); print $3}' "$WORK/list")
"${RENDER[@]}" --generate-credentials --out-dir "$WORK/plan" "$CONFIG" >/dev/null
ok "namespace $NS, $(count "$WORK"/plan/*.yaml) steps"
if k get namespace "$NS" >/dev/null 2>&1; then
  # A kept cluster from an earlier run: its database was initialised with
  # other generated credentials, so start from a clean namespace.
  k delete namespace "$NS" --wait --timeout="${TIMEOUT}s" >/dev/null
  ok "removed $NS left by an earlier run"
fi

# ---------------------------------------------------------------------------
log "Apply step by step"

wait_job() {
  local job=$1 deadline=$((SECONDS + TIMEOUT)) state
  while ((SECONDS < deadline)); do
    state=$(k -n "$NS" get "$job" -o jsonpath='{.status.conditions[?(@.status=="True")].type}')
    case " $state " in
    *" Complete "*) return 0 ;;
    *" Failed "*) fail "$job failed" ;;
    esac
    sleep 2
  done
  fail "$job did not finish within ${TIMEOUT}s"
}

for file in "$WORK"/plan/*.yaml; do
  step=$(basename "$file" .yaml)
  start=$SECONDS
  k apply --server-side --field-manager=heimdall-e2e -f "$file" >/dev/null || fail "apply $step"
  for obj in $(k get -f "$file" -o name); do
    case $obj in
    deployment.apps/* | statefulset.apps/*)
      k -n "$NS" rollout status "$obj" --timeout="${TIMEOUT}s" >/dev/null || fail "$obj not ready" ;;
    job.batch/*) wait_job "$obj" ;;
    esac
  done
  TIMINGS+=("$(printf '%-34s %4ss' "$step" $((SECONDS - start)))")
  ok "$step ($((SECONDS - start))s)"
done

# ---------------------------------------------------------------------------
log "ShopFlow works through the gateway"

for route in api web; do
  accepted=$(k -n "$NS" get httproute "$route" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}')
  [[ $accepted == True ]] || fail "HTTPRoute $route not accepted by the gateway"
done
ok "routes accepted by heimdall-gateway/heimdall"

GW_SVC=$(k -n heimdall-gateway get svc -l gateway.networking.k8s.io/gateway-name=heimdall -o name | head -n1)
[[ -n $GW_SVC ]] || GW_SVC=$(k -n heimdall-gateway get svc -o name | head -n1)
[[ -n $GW_SVC ]] || fail "gateway service not found"
k -n heimdall-gateway port-forward "$GW_SVC" 18080:80 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 30); do curl -s -o /dev/null http://127.0.0.1:18080/ && break; sleep 1; done

get() { curl -fsS --max-time 10 -H "Host: $1" "http://127.0.0.1:18080$2"; }
get "$API_HOST" /health | grep -q '"status":"ok"' || fail "api /health"
ok "api /health through the gateway"
[[ $(get "$API_HOST" /api/products) == '[]' ]] || fail "schema-only database is not empty"
ok "schema-only baseline cloned into the live database; no synthetic records"
get "$WEB_HOST" / | grep -q '<title>ShopFlow</title>' || fail "storefront"
get "$WEB_HOST" /config.js | grep -q "\"apiUrl\":\"http://$API_HOST\"" || fail "runtime config"
ok "storefront serves with the API URL injected at runtime"

[[ $(get "$API_HOST" /api/orders) == '[]' ]] || fail "schema-only orders are not empty"
ok "API reaches PostgreSQL, Redis and RabbitMQ without invented customer records"

# ---------------------------------------------------------------------------
log "Security guarantees on the live cluster"

if k -n "$NS" exec deploy/api -- ls /var/run/secrets/kubernetes.io/serviceaccount >/dev/null 2>&1; then
  fail "a service-account token is mounted in the api pod"
fi
ok "no service-account token in the pod"
[[ $(k -n "$NS" exec deploy/api -- id -u) == 10001 ]] || fail "api does not run as uid 10001"
ok "runs as an unprivileged user"
if k -n "$NS" exec deploy/api -- touch /app/x 2>/dev/null; then fail "root filesystem is writable"; fi
ok "root filesystem is read-only"

# Server-side dry runs pass through admission without persisting anything;
# check that the rejection comes from the guardrail, not from something else.
rejected_by() { # pattern, command...
  local pattern=$1 out
  shift
  if out=$("$@" 2>&1); then return 1; fi
  grep -q "$pattern" <<<"$out" || { echo "$out" >&2; return 1; }
}
rejected_by 'violates PodSecurity "restricted' k -n "$NS" run privileged --image="$PROBE_IMAGE" --dry-run=server \
  --overrides='{"spec":{"containers":[{"name":"p","image":"'"$PROBE_IMAGE"'","securityContext":{"privileged":true}}]}}' ||
  fail "Pod Security did not reject a privileged pod"
ok "Pod Security 'restricted' rejects a privileged pod"
rejected_by 'exceeded quota' k -n "$NS" create service nodeport exposed --tcp=80 --dry-run=server ||
  fail "quota did not reject a NodePort service"
ok "quota rejects NodePort services"

# Each denial is paired with a positive control, so a probe that fails for an
# unrelated reason (image pull, DNS, timeout) cannot pass as "blocked".
k create namespace e2e-outsider --dry-run=client -o yaml | k apply -f - >/dev/null
outsider() { # url: succeeds if any HTTP response arrives from e2e-outsider
  k -n e2e-outsider run "probe-$RANDOM" --rm -i --restart=Never --image="$PROBE_IMAGE" --command -- \
    curl -sS -o /dev/null --max-time 5 "$1" >/dev/null 2>&1
}
inside() { # url: succeeds if the api pod gets any HTTP response
  k -n "$NS" exec deploy/api -- node -e \
    'fetch(process.argv[1],{signal:AbortSignal.timeout(4000)}).then(()=>process.exit(0),()=>process.exit(1))' "$1" 2>/dev/null
}
outsider "http://${GW_SVC#service/}.heimdall-gateway.svc/" || fail "control: the outsider probe cannot reach the gateway"
if outsider "http://api.$NS.svc:8080/health"; then fail "a pod in another namespace reached the preview"; fi
ok "other namespaces cannot reach the preview (default-deny ingress; control probe reached the gateway)"
inside "http://api:8080/health" || fail "control: the api pod cannot reach itself"
if outsider "http://1.1.1.1/"; then
  if inside "http://1.1.1.1/"; then fail "the preview reached the internet without an egress allowlist"; fi
  ok "no egress outside the cluster (default-deny egress; the cluster itself has internet access)"
else
  ok "egress check skipped: this cluster has no internet access to compare against"
fi

bash "$HERE/agent-rbac.sh" "kind-$CLUSTER" "$HEIMDALL" "$CONFIG"

# ---------------------------------------------------------------------------
log "Idempotency and determinism"

for file in "$WORK"/plan/*.yaml; do
  k diff --server-side --field-manager=heimdall-e2e -f "$file" >"$WORK/diff" 2>&1 ||
    { cat "$WORK/diff"; fail "re-applying $(basename "$file") would change the cluster"; }
done
ok "re-applying every step is a no-op"
"${RENDER[@]}" --out-dir "$WORK/a" "$CONFIG" >/dev/null
"${RENDER[@]}" --out-dir "$WORK/b" "$CONFIG" >/dev/null
diff -r "$WORK/a" "$WORK/b" >/dev/null || fail "rendering is not deterministic"
ok "rendering twice gives identical output"

log "PASS: ShopFlow is Ready on kind"
printf '    step timings (baseline for the time-to-ready SLO, P2):\n'
printf '      %s\n' "${TIMINGS[@]}"
