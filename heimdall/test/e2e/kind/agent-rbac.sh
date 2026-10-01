#!/usr/bin/env bash
# Checks the heimdall-agent chart's permissions on a live cluster by
# impersonating the agent's service account:
#   - everything the agent must not do is denied, by the intended layer
#     (RBAC or the ValidatingAdmissionPolicies);
#   - what it is granted is sufficient: it can create a preview namespace,
#     bind its role there, and apply a complete rendered preview.
#
# Usage: agent-rbac.sh <kube-context> <path to heimdall binary> <config>
# Called by run.sh; also runs alone against any disposable cluster with
# Gateway API CRDs installed.
set -euo pipefail
export MSYS_NO_PATHCONV=1

CTX=$1 HEIMDALL=$2 CONFIG=$3
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && (pwd -W 2>/dev/null || pwd))
k() { kubectl --context "$CTX" "$@"; }
AGENT_NS=heimdall-system
SA=system:serviceaccount:$AGENT_NS:heimdall-agent
AS=(--as="$SA" --as-group=system:serviceaccounts --as-group="system:serviceaccounts:$AGENT_NS" --as-group=system:authenticated)
WORK=$(cd "$(mktemp -d)" && (pwd -W 2>/dev/null || pwd))
PREVIEW=""

ok() { printf '    \033[32mok\033[0m %s\n' "$*"; }
count() { echo $#; }
fail() {
  printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2
  exit 1
}
cleanup() {
  [[ -n $PREVIEW ]] && k delete namespace "$PREVIEW" --wait=false >/dev/null 2>&1 || true
  k delete namespace heimdall-rbac-unlabelled heimdall-rbac-probe --wait=false >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# denied <reason pattern> <description> -- command...: the command must fail
# with an error matching the pattern (so it fails for the intended reason).
denied() {
  local pattern=$1 what=$2 out
  shift 3
  if out=$("$@" 2>&1); then fail "agent was allowed to $what"; fi
  grep -Eq "$pattern" <<<"$out" || { echo "$out" >&2; fail "agent could not $what, but not for the expected reason"; }
  ok "cannot $what"
}
agent() { k "${AS[@]}" "$@"; }
cannot() { # verb resource [flags]: RBAC must not grant it at all
  if agent auth can-i "$@" --quiet 2>/dev/null; then fail "agent can $*"; fi
  ok "cannot $*"
}

namespace_yaml() { # name, labelled?
  printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n' "$1"
  [[ $2 == yes ]] && printf '  labels:\n    heimdall.dev/preview: "true"\n'
  return 0
}

printf '\n\033[1;34m==> Agent chart: install and permission checks\033[0m\n'
helm upgrade --install heimdall-agent "$ROOT/charts/heimdall-agent" --kube-context "$CTX" \
  --namespace "$AGENT_NS" --create-namespace --wait=false >/dev/null
ok "chart installed (the agent image arrives in P3; only its permissions are exercised here)"

VAP="ValidatingAdmissionPolicy '[^']+' with binding '[^']+' denied request"

# Admission policies take effect a moment after they are created. Wait until
# one is observably enforced: the same self-check the agent must pass before
# doing any work (P3), so it never acts during that window.
enforced=0
for _ in $(seq 60); do
  if ! out=$(agent create --dry-run=server -f - < <(namespace_yaml heimdall-rbac-unlabelled no) 2>&1) &&
    grep -Eq "$VAP" <<<"$out"; then
    enforced=1
    break
  fi
  sleep 1
done
((enforced)) || fail "the agent's admission policies are not enforced after 60s"
ok "admission policies are enforced"

# --- Denied ---------------------------------------------------------------
denied "$VAP" "create a namespace without the preview label" -- \
  agent create -f - < <(namespace_yaml heimdall-rbac-unlabelled no)
denied "$VAP" "create a labelled namespace outside the prefix" -- \
  agent create -f - < <(namespace_yaml rogue-preview yes)
denied "$VAP" "label kube-system as a preview" -- \
  agent label namespace kube-system heimdall.dev/preview=true
# A namespace with the right prefix that an admin created is not a preview:
# the agent may neither adopt it (by labelling) nor delete it.
k create namespace heimdall-rbac-probe --dry-run=client -o yaml | k apply -f - >/dev/null
denied "$VAP" "adopt an admin's heimdall-* namespace by labelling it" -- \
  agent label namespace heimdall-rbac-probe heimdall.dev/preview=true
denied "$VAP" "delete an admin's heimdall-* namespace" -- agent delete namespace heimdall-rbac-probe --dry-run=server
denied "$VAP" "delete its own namespace" -- agent delete namespace "$AGENT_NS" --dry-run=server
denied "forbidden" "read secrets in kube-system" -- agent -n kube-system get secrets
denied "forbidden" "read config maps cluster-wide" -- agent get configmaps -A
# (TYPE/NAME in can-i means a named object, so subresources need --subresource.)
cannot create pods --subresource=exec --all-namespaces
cannot create pods --subresource=portforward --all-namespaces
cannot get pods --subresource=log -n kube-system
cannot create clusterrolebindings
denied "$VAP|forbidden" "bind its role in kube-system" -- \
  agent -n kube-system create rolebinding x --clusterrole=heimdall-agent-preview-manager --user=x --dry-run=server
denied "forbidden" "create deployments outside preview namespaces" -- \
  agent -n kube-system create deployment x --image=busybox --dry-run=server

# --- Allowed: a full preview --------------------------------------------
"$HEIMDALL" render --placeholder-images --generate-credentials --repo acme/shopflow --pr 185 \
  --out-dir "$WORK/plan" "$CONFIG" >/dev/null 2>&1
PREVIEW=$("$HEIMDALL" render --placeholder-images --repo acme/shopflow --pr 185 --list "$CONFIG" 2>/dev/null | awk '/^namespace:/ {print $2}')

agent create -f - < <(namespace_yaml "$PREVIEW" yes) >/dev/null || fail "agent cannot create preview namespace $PREVIEW"
ok "creates preview namespace $PREVIEW"
denied "forbidden" "act in the new namespace before binding its role" -- agent -n "$PREVIEW" get secrets
denied "$VAP|forbidden" "bind cluster-admin in a preview namespace" -- \
  agent -n "$PREVIEW" create rolebinding admin --clusterrole=cluster-admin --serviceaccount="$AGENT_NS:heimdall-agent" --dry-run=server
agent -n "$PREVIEW" create rolebinding heimdall-agent --clusterrole=heimdall-agent-preview-manager \
  --serviceaccount="$AGENT_NS:heimdall-agent" >/dev/null || fail "agent cannot bind its role in $PREVIEW"
ok "binds heimdall-agent-preview-manager in $PREVIEW"

# Server-side dry run: full admission and authorization, nothing persisted
# (placeholder images would never start anyway).
for file in "$WORK"/plan/*.yaml; do
  agent apply --server-side --dry-run=server --field-manager=heimdall-agent -f "$file" >/dev/null ||
    fail "agent cannot apply $(basename "$file")"
done
ok "can apply every step of a rendered preview ($(count "$WORK"/plan/*.yaml) files)"
agent -n "$PREVIEW" auth can-i list pods --quiet || fail "agent cannot read pods in $PREVIEW"
agent -n "$PREVIEW" auth can-i get pods --subresource=log --quiet || fail "agent cannot read logs in $PREVIEW"
ok "can read pods and logs in $PREVIEW"
agent delete namespace "$PREVIEW" --wait=false >/dev/null || fail "agent cannot delete $PREVIEW"
PREVIEW=""
ok "deletes its preview namespace"
