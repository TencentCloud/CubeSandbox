#!/bin/sh
# Guard: the rendered CubeMaster config must carry the configured master
# replica count in common.default_headless_service_nodes_num.
#
# The chart does not deploy a headless Service, so HealthyMasterNodes() reads
# this static value. A hardcoded 1 makes every Master under-estimate the global
# create concurrency when controlPlane.master.replicas > 1.

set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); echo "FAIL: $*" >&2; }

render() { # render <name> [extra sets...]
  local name="$1"; shift
  helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >"$TMP_DIR/$name.yaml"
}

master_nodes() { # master_nodes <rendered-file>
  grep -E '^[[:space:]]*default_headless_service_nodes_num:' "$1" \
    | head -1 | awk '{print $2}'
}

render default
# Test-only override: the chart rejects a multi-replica Master on a
# ReadWriteOnce artifact PVC (the second Pod cannot attach it). This does not
# change the chart's storage behavior; it only lets the replica-count render
# case reach the config template.
render replicas-3 \
  --set controlPlane.master.replicas=3 \
  --set 'controlPlane.master.persistence.accessModes={ReadWriteMany}'

default_nodes="$(master_nodes "$TMP_DIR/default.yaml")"
three_nodes="$(master_nodes "$TMP_DIR/replicas-3.yaml")"

[ "$default_nodes" = "1" ] && ok || bad "default replicas: got '$default_nodes', want '1'"
[ "$three_nodes" = "3" ] && ok || bad "replicas=3: got '$three_nodes', want '3'"

if [ "$FAIL" -ne 0 ]; then
  echo "master-replicas-config: $PASS passed, $FAIL failed" >&2
  exit 1
fi
echo "master-replicas-config: $PASS passed"
