#!/bin/sh
# Guard: rendered master config follows the master replica count, and the
# pod-template checksum moves only when that config changes.

set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Pinned so the conf checksum stays stable across renders.
COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test --set-string lifecycleManager.adminToken=test-admin-token-16ch"
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); echo "FAIL: $*" >&2; }

render() { # render <name> [extra sets...]
  local name="$1"; shift
  # Fixed release name so the checksum only tracks the values under test.
  helm template master-replicas "$CHART_DIR" $COMMON_SETS "$@" >"$TMP_DIR/$name.yaml"
}

master_nodes() { # master_nodes <rendered-file>
  grep -E '^[[:space:]]*default_headless_service_nodes_num:' "$1" \
    | head -1 | awk '{print $2}'
}

master_checksum() { # master_checksum <rendered-file>
  grep -E '^[[:space:]]*checksum/config:' "$1" \
    | head -1 | awk '{print $2}' | tr -d '"'
}

render default
# Shared volume so multiple masters pass chart validation.
render replicas-3 \
  --set controlPlane.master.replicas=3 \
  --set 'controlPlane.master.persistence.accessModes={ReadWriteMany}'
render port-8090 --set controlPlane.master.service.port=8090
render api-replicas --set controlPlane.api.replicas=2

default_nodes="$(master_nodes "$TMP_DIR/default.yaml")"
three_nodes="$(master_nodes "$TMP_DIR/replicas-3.yaml")"
default_sum="$(master_checksum "$TMP_DIR/default.yaml")"
three_sum="$(master_checksum "$TMP_DIR/replicas-3.yaml")"
port_sum="$(master_checksum "$TMP_DIR/port-8090.yaml")"
api_sum="$(master_checksum "$TMP_DIR/api-replicas.yaml")"

[ "$default_nodes" = "1" ] && ok || bad "default replicas: got '$default_nodes', want '1'"
[ "$three_nodes" = "3" ] && ok || bad "replicas=3: got '$three_nodes', want '3'"
printf '%s' "$default_sum" | grep -Eq '^[0-9a-f]{64}$' \
  && ok || bad "default checksum: got '$default_sum', want 64 hex chars"
[ "$three_sum" != "$default_sum" ] && ok || bad "replicas=3 checksum matched the default ($default_sum)"
[ "$port_sum" != "$default_sum" ] && ok || bad "service.port=8090 checksum matched the default ($default_sum)"
[ "$api_sum" = "$default_sum" ] && ok || bad "api.replicas=2 changed the master checksum: got '$api_sum', want '$default_sum'"

if [ "$FAIL" -ne 0 ]; then
  echo "master-replicas-config: $PASS passed, $FAIL failed" >&2
  exit 1
fi
echo "master-replicas-config: $PASS passed"
