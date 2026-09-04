#!/bin/sh
# Guard: top-level redis.db is rendered into CubeMaster conf and
# Proxy / LCM / CubeOps env so control-plane components share one logical
# Redis DB. Nested cubeProxy.redis.db / lifecycleManager.redis.db are ignored.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

assert_redis_db_env() {
  local file="$1"
  local want="$2"
  local name="$3"
  grep -E "name: ${name}" -A1 "$file" | grep -q "\"${want}\"" || {
    echo "${name} missing value ${want}" >&2
    exit 1
  }
}

# Default redis.db is 0 for Master / Proxy / LCM / Ops.
helm template redis-db-default "$CHART_DIR" $COMMON_SETS \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeOps.enabled=true \
  > "$TMP_DIR/default.yaml"

grep -q 'db_no: 0' "$TMP_DIR/default.yaml" || {
  echo "CubeMaster conf missing default db_no: 0" >&2
  exit 1
}
assert_redis_db_env "$TMP_DIR/default.yaml" 0 REDIS_DB
assert_redis_db_env "$TMP_DIR/default.yaml" 0 CUBE_PROXY_REGISTRY_REDIS_DB
assert_redis_db_env "$TMP_DIR/default.yaml" 0 CUBE_LCM_REDIS_DB
# Proxy + Ops both emit REDIS_DB.
redis_db_count="$(grep -c 'name: REDIS_DB' "$TMP_DIR/default.yaml" || true)"
[ "$redis_db_count" -ge 2 ] || {
  echo "expected REDIS_DB on both CubeProxy and CubeOps, got ${redis_db_count}" >&2
  exit 1
}

# redis.db=3 overrides Master / Proxy / LCM / Ops together; nested db values are ignored.
helm template redis-db-override "$CHART_DIR" $COMMON_SETS \
  --set redis.db=3 \
  --set cubeProxy.enabled=true \
  --set cubeProxy.redis.db=9 \
  --set lifecycleManager.enabled=true \
  --set lifecycleManager.redis.db=9 \
  --set cubeOps.enabled=true \
  > "$TMP_DIR/override.yaml"

grep -q 'db_no: 3' "$TMP_DIR/override.yaml" || {
  echo "CubeMaster conf missing db_no: 3" >&2
  exit 1
}
assert_redis_db_env "$TMP_DIR/override.yaml" 3 REDIS_DB
assert_redis_db_env "$TMP_DIR/override.yaml" 3 CUBE_PROXY_REGISTRY_REDIS_DB
assert_redis_db_env "$TMP_DIR/override.yaml" 3 CUBE_LCM_REDIS_DB
if grep -E 'name: (REDIS_DB|CUBE_PROXY_REGISTRY_REDIS_DB|CUBE_LCM_REDIS_DB)' -A1 "$TMP_DIR/override.yaml" | grep -q '"9"'; then
  echo "nested redis.db leaked into rendered env" >&2
  exit 1
fi

echo "ok: redis.db wires Master/Proxy/LCM/Ops and ignores nested db"
