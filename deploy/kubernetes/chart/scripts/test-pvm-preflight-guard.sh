#!/usr/bin/env bash
# Guard: the PVM preflight Hook must render from values alone (no cluster lookup).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON=(
  --set-string mysql.password=test
  --set-string mysql.rootPassword=test
  --set-string redis.password=test
)

render() {
  helm template preflight-guard "$CHART_DIR" "${COMMON[@]}" "$@"
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Offline rendering must still emit the Hook: gating it on a cluster lookup
# silently drops the startup-gate safety net for helm template / GitOps.
render -s templates/pvm-startup-gate-preflight.yaml > "$TMP_DIR/hook.yaml" \
  || fail "the PVM preflight Hook did not render"
for kind in ServiceAccount ClusterRole ClusterRoleBinding Role RoleBinding Job; do
  grep -q "^kind: ${kind}$" "$TMP_DIR/hook.yaml" || fail "missing ${kind} in the PVM preflight Hook"
done
grep -q 'name: NODE_SELECTOR' "$TMP_DIR/hook.yaml" || fail "preflight Job is missing NODE_SELECTOR"
grep -q 'helm.sh/hook: pre-install,pre-upgrade' "$TMP_DIR/hook.yaml" \
  || fail "preflight Hook annotations changed"

# The consistency check must stay value-driven and fail at render time.
if render --set bootstrap.pvmHostKernel.startupGate.requirePreflight=false \
    >/dev/null 2> "$TMP_DIR/err"; then
  fail "requirePreflight=false must be rejected at render time"
fi
grep -q 'requires startupGate.requirePreflight=true' "$TMP_DIR/err" \
  || fail "unexpected requirePreflight error message"

# No cube-node -> no PVM preflight at all.
render --set cubeNode.enabled=false > "$TMP_DIR/no-cube-node.yaml"
if grep -q 'component: pvm-preflight' "$TMP_DIR/no-cube-node.yaml"; then
  fail "cubeNode.enabled=false must not render the PVM preflight Hook"
fi

echo "pvm preflight guard passed"
