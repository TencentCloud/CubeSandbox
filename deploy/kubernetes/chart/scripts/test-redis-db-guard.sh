#!/bin/sh
# Guard: top-level redis.db is rendered into CubeMaster conf, CubeTemplateCenter
# conf and Proxy / LCM / CubeOps env so control-plane components share one
# logical Redis DB. Non-zero nested cubeProxy.redis.db / lifecycleManager.redis.db
# fail render (zero is tolerated for helm upgrade leftover defaults).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

expect_fail() {
  name="$1"
  err_file="$2"
  needle="$3"
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

# Assert every occurrence of env name carries the expected value.
assert_all_redis_db_env() {
  file="$1"
  want="$2"
  name="$3"
  awk -v name="$name" -v want="$want" '
    $0 ~ ("name: " name "$") {
      getline
      if ($0 !~ ("value: \"" want "\"")) {
        printf "%s occurrence %d missing value %s (got: %s)\n", name, ++n, want, $0 > "/dev/stderr"
        bad=1
      } else {
        n++
      }
    }
    END {
      if (n < 1) {
        printf "%s missing entirely (wanted value %s)\n", name, want > "/dev/stderr"
        exit 1
      }
      if (bad) exit 1
    }
  ' "$file"
}

# Default redis.db is 0 for Master / TC / Proxy / LCM / Ops.
helm template redis-db-default "$CHART_DIR" $COMMON_SETS \
  --set controlPlane.enabled=true \
  --set placement.controlPlane.nodeSelector.role=control \
  --set placement.compute.nodeSelector.role=compute \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeOps.enabled=true \
  > "$TMP_DIR/default.yaml"

grep -Eq '^[[:space:]]*db_no: 0[[:space:]]*$' "$TMP_DIR/default.yaml" || {
  echo "CubeMaster conf missing default db_no: 0" >&2
  exit 1
}
# TC conf renders three redis blocks (redis / redis_read / redis_write);
# Master renders one. All four must carry the default.
db_no_zero_count="$(grep -Ec '^[[:space:]]*db_no: 0[[:space:]]*$' "$TMP_DIR/default.yaml" || true)"
[ "$db_no_zero_count" -ge 4 ] || {
  echo "expected >=4 db_no: 0 occurrences (Master 1 + TC 3), got ${db_no_zero_count}" >&2
  exit 1
}
assert_all_redis_db_env "$TMP_DIR/default.yaml" 0 REDIS_DB
assert_all_redis_db_env "$TMP_DIR/default.yaml" 0 CUBE_PROXY_REGISTRY_REDIS_DB
assert_all_redis_db_env "$TMP_DIR/default.yaml" 0 CUBE_LCM_REDIS_DB
# Proxy + Ops both emit REDIS_DB.
redis_db_count="$(grep -c 'name: REDIS_DB' "$TMP_DIR/default.yaml" || true)"
[ "$redis_db_count" -ge 2 ] || {
  echo "expected REDIS_DB on both CubeProxy and CubeOps, got ${redis_db_count}" >&2
  exit 1
}

# redis.db=3 overrides Master / TC / Proxy / LCM / Ops together.
helm template redis-db-override "$CHART_DIR" $COMMON_SETS \
  --set redis.db=3 \
  --set controlPlane.enabled=true \
  --set placement.controlPlane.nodeSelector.role=control \
  --set placement.compute.nodeSelector.role=compute \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeOps.enabled=true \
  > "$TMP_DIR/override.yaml"

grep -Eq '^[[:space:]]*db_no: 3[[:space:]]*$' "$TMP_DIR/override.yaml" || {
  echo "CubeMaster conf missing db_no: 3" >&2
  exit 1
}
# No block may stay on the old default: Master renders 1 redis block and TC
# renders 3, so db_no: 3 must cover all four and db_no: 0 must be gone.
db_no_three_count="$(grep -Ec '^[[:space:]]*db_no: 3[[:space:]]*$' "$TMP_DIR/override.yaml" || true)"
[ "$db_no_three_count" -ge 4 ] || {
  echo "expected >=4 db_no: 3 occurrences (Master 1 + TC 3), got ${db_no_three_count}" >&2
  exit 1
}
if grep -Eq '^[[:space:]]*db_no: 0[[:space:]]*$' "$TMP_DIR/override.yaml"; then
  echo "a conf still renders db_no: 0 under redis.db=3 (TC conf must follow cube.redisDB)" >&2
  exit 1
fi
assert_all_redis_db_env "$TMP_DIR/override.yaml" 3 REDIS_DB
assert_all_redis_db_env "$TMP_DIR/override.yaml" 3 CUBE_PROXY_REGISTRY_REDIS_DB
assert_all_redis_db_env "$TMP_DIR/override.yaml" 3 CUBE_LCM_REDIS_DB

# Non-zero nested keys must fail render (not silently ignored).
expect_fail proxy-redis-db-removed "$TMP_DIR/proxy-db.err" \
  'cubeProxy.redis.db was removed' \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeProxy.redis.db=9

expect_fail lcm-redis-db-removed "$TMP_DIR/lcm-db.err" \
  'lifecycleManager.redis.db was removed' \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set lifecycleManager.redis.db=9

# Guards must run even when CubeOps is disabled.
expect_fail proxy-redis-db-no-ops "$TMP_DIR/proxy-db-noops.err" \
  'cubeProxy.redis.db was removed' \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeOps.enabled=false \
  --set webui.enabled=false \
  --set cubeNode.enabled=false \
  --set cubeProxy.redis.db=9

# Leftover nested db:0 from a prior chart default must not block helm upgrade.
helm template redis-db-upgrade-leftover "$CHART_DIR" $COMMON_SETS \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeOps.enabled=false \
  --set webui.enabled=false \
  --set cubeNode.enabled=false \
  --set cubeProxy.redis.db=0 \
  --set lifecycleManager.redis.db=0 \
  > "$TMP_DIR/upgrade-leftover.yaml"

grep -Eq '^[[:space:]]*db_no: 0[[:space:]]*$' "$TMP_DIR/upgrade-leftover.yaml" || {
  echo "upgrade leftover db:0 should still render Master db_no: 0" >&2
  exit 1
}

# Leftover nested db:0 must not block overriding to a non-zero top-level db.
helm template redis-db-leftover-override "$CHART_DIR" $COMMON_SETS \
  --set redis.db=3 \
  --set cubeProxy.enabled=true \
  --set lifecycleManager.enabled=true \
  --set cubeProxy.redis.db=0 \
  --set lifecycleManager.redis.db=0 \
  > "$TMP_DIR/leftover-override.yaml"

grep -Eq '^[[:space:]]*db_no: 3[[:space:]]*$' "$TMP_DIR/leftover-override.yaml" || {
  echo "leftover db:0 + redis.db=3 should render Master db_no: 3" >&2
  exit 1
}
assert_all_redis_db_env "$TMP_DIR/leftover-override.yaml" 3 REDIS_DB
assert_all_redis_db_env "$TMP_DIR/leftover-override.yaml" 3 CUBE_PROXY_REGISTRY_REDIS_DB
assert_all_redis_db_env "$TMP_DIR/leftover-override.yaml" 3 CUBE_LCM_REDIS_DB

# Invalid redis.db values must fail at render time.
expect_fail redis-db-non-integer "$TMP_DIR/db-foo.err" \
  'redis.db must be an integer' \
  --set redis.db=foo

expect_fail redis-db-negative "$TMP_DIR/db-neg.err" \
  'redis.db must be an integer' \
  --set redis.db=-1

expect_fail redis-db-out-of-range "$TMP_DIR/db-16.err" \
  'redis.db must be an integer' \
  --set redis.db=16

echo "ok: redis.db wires Master/TC/Proxy/LCM/Ops; non-zero nested db fails even without Ops"
