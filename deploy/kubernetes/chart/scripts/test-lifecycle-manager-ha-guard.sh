#!/bin/sh
# Guard: lifecycleManager.replicas>=2 requires leader election as a real
# bool; renewInterval*2 must be < leaseTTL; retryInterval must be < leaseTTL.
# Single-replica election is allowed (image-first upgrades).
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

# Two replicas without election must fail (dual-leader).
expect_fail clm-ha-replicas-no-election "$TMP_DIR/no-election.err" \
  'lifecycleManager.replicas>=2 requires lifecycleManager.leaderElection.enabled=true' \
  --set lifecycleManager.replicas=2 \
  --set lifecycleManager.leaderElection.enabled=false

# --set-string "false" is a non-empty string and must not look enabled.
expect_fail clm-ha-replicas-set-string-false "$TMP_DIR/set-string-false.err" \
  'lifecycleManager.leaderElection.enabled must be a boolean' \
  --set lifecycleManager.replicas=2 \
  --set-string lifecycleManager.leaderElection.enabled=false

# Non-integer-second durations must fail.
expect_fail clm-ha-renew-unit "$TMP_DIR/unit.err" \
  'leaseTTL, renewInterval, and retryInterval must be integer seconds' \
  --set lifecycleManager.replicas=2 \
  --set lifecycleManager.leaderElection.enabled=true \
  --set-string lifecycleManager.leaderElection.leaseTTL=10s \
  --set-string lifecycleManager.leaderElection.renewInterval=500ms \
  --set-string lifecycleManager.leaderElection.retryInterval=1s

# renewInterval >= leaseTTL must fail (also violates renew*2 < lease).
expect_fail clm-ha-renew-too-long "$TMP_DIR/renew.err" \
  'lifecycleManager.leaderElection.renewInterval must be less than half of leaseTTL' \
  --set lifecycleManager.replicas=2 \
  --set lifecycleManager.leaderElection.enabled=true \
  --set-string lifecycleManager.leaderElection.leaseTTL=10s \
  --set-string lifecycleManager.leaderElection.renewInterval=10s \
  --set-string lifecycleManager.leaderElection.retryInterval=1s

# renew*2 >= lease (5s * 2 == 10s) must fail even though renew < lease.
expect_fail clm-ha-renew-not-half "$TMP_DIR/renew-half.err" \
  'lifecycleManager.leaderElection.renewInterval must be less than half of leaseTTL' \
  --set lifecycleManager.replicas=2 \
  --set lifecycleManager.leaderElection.enabled=true \
  --set-string lifecycleManager.leaderElection.leaseTTL=10s \
  --set-string lifecycleManager.leaderElection.renewInterval=5s \
  --set-string lifecycleManager.leaderElection.retryInterval=1s

# retryInterval >= leaseTTL must fail.
expect_fail clm-ha-retry-too-long "$TMP_DIR/retry.err" \
  'lifecycleManager.leaderElection.retryInterval must be less than leaseTTL' \
  --set lifecycleManager.replicas=2 \
  --set lifecycleManager.leaderElection.enabled=true \
  --set-string lifecycleManager.leaderElection.leaseTTL=10s \
  --set-string lifecycleManager.leaderElection.renewInterval=3s \
  --set-string lifecycleManager.leaderElection.retryInterval=10s

# Single replica with election enabled must render (image-first upgrade).
if ! helm template clm-ha-single-election "$CHART_DIR" $COMMON_SETS \
  --show-only templates/lifecycle-manager.yaml \
  --set lifecycleManager.replicas=1 \
  --set lifecycleManager.leaderElection.enabled=true \
  >/dev/null 2>"$TMP_DIR/single.err"; then
  echo "expected success: clm-ha-single-election" >&2
  cat "$TMP_DIR/single.err" >&2
  exit 1
fi

# Default HA values render two replicas and enable election.
helm template clm-ha-default "$CHART_DIR" $COMMON_SETS \
  --show-only templates/lifecycle-manager.yaml \
  >"$TMP_DIR/default.yaml" 2>"$TMP_DIR/default.err" || {
  echo "expected success: clm-ha-default" >&2
  cat "$TMP_DIR/default.err" >&2
  exit 1
}
grep -q 'replicas: 2' "$TMP_DIR/default.yaml" || {
  echo "expected lifecycle-manager replicas: 2 in default render" >&2
  exit 1
}
grep -A1 'name: CUBE_LCM_LEADER_ELECTION_ENABLED' "$TMP_DIR/default.yaml" | grep -q 'value: "true"' || {
  echo "CUBE_LCM_LEADER_ELECTION_ENABLED is not true" >&2
  exit 1
}

echo "ok: lifecycle-manager HA guards"
