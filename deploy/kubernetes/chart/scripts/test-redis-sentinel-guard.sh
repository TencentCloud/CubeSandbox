#!/bin/sh
# Guard: redis.masterName enables external Redis Sentinel for CubeMaster /
# CubeProxy / CLM, skips the chart-managed Redis StatefulSet, and wires
# master_name / sentinel_nodes into conf + env.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

expect_fail() {
  local name="$1"
  local err_file="$2"
  local needle="$3"
  shift 3
  if helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >/dev/null 2>"$err_file"; then
    echo "expected fail: $name" >&2
    exit 1
  fi
  grep -qi "$needle" "$err_file" || {
    echo "unexpected error for $name (wanted /$needle/):" >&2
    cat "$err_file" >&2
    exit 1
  }
}

# masterName without sentinelNodes must fail.
expect_fail redis-sentinel-nodes-missing "$TMP_DIR/missing.err" \
  'redis.masterName requires redis.sentinelNodes' \
  --set-string redis.masterName=mymaster

# Whitespace-only / comma-only sentinelNodes must fail (no valid endpoint).
expect_fail redis-sentinel-nodes-blank "$TMP_DIR/blank.err" \
  'redis.masterName requires redis.sentinelNodes' \
  --set-string redis.masterName=mymaster \
  --set-string redis.sentinelNodes='   '

cat > "$TMP_DIR/commas-values.yaml" <<'EOF'
redis:
  password: test
  masterName: mymaster
  sentinelNodes: ", , ,"
mysql:
  password: test
  rootPassword: test
EOF
if helm template redis-sentinel-nodes-commas "$CHART_DIR" -f "$TMP_DIR/commas-values.yaml" \
  >/dev/null 2>"$TMP_DIR/commas.err"; then
  echo "expected fail: redis-sentinel-nodes-commas" >&2
  exit 1
fi
grep -qi 'redis.masterName requires redis.sentinelNodes' "$TMP_DIR/commas.err" || {
  echo "unexpected error for commas-only sentinelNodes:" >&2
  cat "$TMP_DIR/commas.err" >&2
  exit 1
}

# sentinelNodes without masterName must fail.
expect_fail redis-sentinel-master-missing "$TMP_DIR/master-missing.err" \
  'redis.sentinelNodes requires redis.masterName' \
  --set-string redis.sentinelNodes='10.0.0.11:26379'

# Mixed Sentinel + standalone host must fail.
expect_fail redis-sentinel-mixed-host "$TMP_DIR/mixed.err" \
  'cannot be combined with redis.host' \
  --set-string redis.masterName=mymaster \
  --set-string redis.sentinelNodes='10.0.0.11:26379' \
  --set-string redis.host='redis.example.internal'

# Sentinel mode renders master conf fields and skips built-in Redis.
cat > "$TMP_DIR/sentinel-values.yaml" <<'EOF'
redis:
  password: test
  masterName: mymaster
  sentinelNodes: "10.0.0.11:26379,10.0.0.12:26379"
  sentinelPassword: sentpass
cubeProxy:
  enabled: false
lifecycleManager:
  enabled: false
mysql:
  password: test
  rootPassword: test
EOF
helm template redis-sentinel-ok "$CHART_DIR" -f "$TMP_DIR/sentinel-values.yaml" \
  > "$TMP_DIR/sentinel.yaml"

grep -q 'master_name: "mymaster"' "$TMP_DIR/sentinel.yaml" || {
  echo "CubeMaster conf missing master_name" >&2
  exit 1
}
grep -q 'sentinel_nodes: "10.0.0.11:26379,10.0.0.12:26379"' "$TMP_DIR/sentinel.yaml" || {
  echo "CubeMaster conf missing sentinel_nodes" >&2
  exit 1
}
grep -q 'sentinel_password: "sentpass"' "$TMP_DIR/sentinel.yaml" || {
  echo "CubeMaster conf missing sentinel_password" >&2
  exit 1
}
grep -q 'nodes: ""' "$TMP_DIR/sentinel.yaml" || {
  echo "Sentinel mode should leave redis.nodes empty" >&2
  exit 1
}
if awk '
  BEGIN { want=0 }
  /^kind: StatefulSet$/ { want=1; next }
  want && /app.kubernetes.io\/component: redis/ { found=1 }
  /^---$/ { want=0 }
  END { exit found ? 0 : 1 }
' "$TMP_DIR/sentinel.yaml"; then
  echo "redis.masterName must skip built-in Redis StatefulSet" >&2
  exit 1
fi

# Proxy + CLM get Sentinel env when enabled.
cat > "$TMP_DIR/proxy-values.yaml" <<'EOF'
redis:
  password: test
  masterName: mymaster
  sentinelNodes: "10.0.0.11:26379"
mysql:
  password: test
  rootPassword: test
placement:
  controlPlane:
    nodeSelector:
      role: control
EOF
helm template redis-sentinel-proxy "$CHART_DIR" -f "$TMP_DIR/proxy-values.yaml" \
  > "$TMP_DIR/proxy.yaml"

grep -q 'name: REDIS_MASTER_NAME' "$TMP_DIR/proxy.yaml" || {
  echo "CubeProxy missing REDIS_MASTER_NAME" >&2
  exit 1
}
grep -q 'value: "mymaster"' "$TMP_DIR/proxy.yaml" || {
  echo "CubeProxy REDIS_MASTER_NAME value missing" >&2
  exit 1
}
grep -q 'name: CUBE_LCM_REDIS_MASTER_NAME' "$TMP_DIR/proxy.yaml" || {
  echo "CLM missing CUBE_LCM_REDIS_MASTER_NAME" >&2
  exit 1
}
grep -q 'name: CUBE_PROXY_REGISTRY_REDIS_SENTINEL_NODES' "$TMP_DIR/proxy.yaml" || {
  echo "CubeProxy registry missing SENTINEL_NODES" >&2
  exit 1
}
grep -q 'key: redis-sentinel-password' "$TMP_DIR/proxy.yaml" || {
  echo "Secret missing redis-sentinel-password key ref" >&2
  exit 1
}

# CubeOps gets REDIS_MASTER_NAME / SENTINEL_NODES / SENTINEL_PASSWORD too.
grep -q 'name: REDIS_SENTINEL_NODES' "$TMP_DIR/proxy.yaml" || {
  echo "CubeOps missing REDIS_SENTINEL_NODES" >&2
  exit 1
}
grep -q 'name: REDIS_SENTINEL_PASSWORD' "$TMP_DIR/proxy.yaml" || {
  echo "CubeOps missing REDIS_SENTINEL_PASSWORD" >&2
  exit 1
}

echo "redis sentinel guard OK"
