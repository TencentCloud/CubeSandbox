#!/usr/bin/env bash
# CubeProxy resolver entrypoint regression tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CUBE_PROXY_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CUBE_PROXY_START_SH="${CUBE_PROXY_DIR}/start.sh"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"
}

run_prepare_resolver() {
  env \
    CUBE_PROXY_START_SH="${CUBE_PROXY_START_SH}" \
    "$@" \
    bash -c '
      set -euo pipefail
      source "${CUBE_PROXY_START_SH}"
      prepare_resolver_include
    '
}

test_cube_proxy_start_discover_resolver_addresses() {
  local resolv_conf="${TMP_DIR}/cube-proxy-resolver.conf"
  local output=""

  cat > "${resolv_conf}" <<'EOF'
nameserver 127.0.0.53
nameserver 100.100.2.136
nameserver 100.100.2.136
nameserver 999.999.999.999
nameserver 0377.0377.0377.0377
nameserver 169.254.254.53
nameserver 2001:db8::1
nameserver 2001:db8::1
nameserver 2001:db8::zz
EOF

  output="$(
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
      bash -c '
        set -euo pipefail
        source "'"${CUBE_PROXY_START_SH}"'"
        discover_resolver_addresses
      '
  )"

  grep -Fxq "127.0.0.53" <<<"${output}" \
    || fail "discover_resolver_addresses should keep container-local loopback resolvers"
  grep -Fxq "100.100.2.136" <<<"${output}" \
    || fail "discover_resolver_addresses should include upstream nameservers"
  grep -Fxq "169.254.254.53" <<<"${output}" \
    || fail "discover_resolver_addresses should include link-local IPv4 nameservers"
  if grep -Fxq "999.999.999.999" <<<"${output}" || grep -Fxq "0377.0377.0377.0377" <<<"${output}"; then
    fail "discover_resolver_addresses should skip invalid IPv4 nameservers"
  fi
  grep -Fxq "[2001:db8::1]" <<<"${output}" \
    || fail "discover_resolver_addresses should include IPv6 nameservers"
  if grep -Fxq "2001:db8::zz" <<<"${output}"; then
    fail "discover_resolver_addresses should skip invalid IPv6 nameservers"
  fi
  if [[ "$(grep -Fxc "100.100.2.136" <<<"${output}")" != "1" ]]; then
    fail "discover_resolver_addresses should dedupe identical nameservers"
  fi
}

test_cube_proxy_start_build_resolver_include() {
  local output=""

  output="$(
    bash -c '
      set -euo pipefail
      source "'"${CUBE_PROXY_START_SH}"'"
      build_cube_proxy_resolver_include "169.254.254.53 100.100.2.136"
    '
  )"
  [[ "${output}" == $'resolver 169.254.254.53 100.100.2.136 ipv6=off valid=30s;\nresolver_timeout 5s;' ]] \
    || fail "build_cube_proxy_resolver_include should render the exact nginx include body (got: ${output})"

  output="$(
    bash -c '
      set -euo pipefail
      source "'"${CUBE_PROXY_START_SH}"'"
      build_cube_proxy_resolver_include ""
    '
  )"
  [[ "${output}" == "# no usable nginx resolver discovered" ]] \
    || fail "build_cube_proxy_resolver_include should render an explicit no-op marker for empty resolver lists"

  output="$(
    CUBE_PROXY_RESOLVER_VALID=300s \
    CUBE_PROXY_RESOLVER_TIMEOUT=10s \
    CUBE_PROXY_RESOLVER_IPV6=on \
      bash -c '
        set -euo pipefail
        source "'"${CUBE_PROXY_START_SH}"'"
        build_cube_proxy_resolver_include "169.254.254.53"
      '
  )"
  [[ "${output}" == $'resolver 169.254.254.53 ipv6=on valid=300s;\nresolver_timeout 10s;' ]] \
    || fail "build_cube_proxy_resolver_include should honor existing resolver tuning env vars"

  if CUBE_PROXY_RESOLVER_VALID='bad;' bash -c '
    set -euo pipefail
    source "'"${CUBE_PROXY_START_SH}"'"
    build_cube_proxy_resolver_include "169.254.254.53"
  ' >/dev/null 2>&1; then
    fail "build_cube_proxy_resolver_include should reject invalid resolver tuning"
  fi
}

test_cube_proxy_start_prepare_resolver_include_for_hostname() {
  local resolv_conf="${TMP_DIR}/cube-proxy-hostname-resolv.conf"
  local global_conf="${TMP_DIR}/cube-proxy-global.conf"
  local resolver_inc="${TMP_DIR}/resolver.inc"

  cat > "${resolv_conf}" <<'EOF'
nameserver 169.254.254.53
nameserver 100.100.2.136
EOF

  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>&1; then
    fail "prepare_resolver_include should succeed for hostname Redis targets when resolvers are available"
  fi

  assert_contains "${resolver_inc}" "resolver 169.254.254.53 100.100.2.136 ipv6=off valid=30s;"
  assert_contains "${resolver_inc}" "resolver_timeout 5s;"
}

test_cube_proxy_start_prepare_resolver_include_for_hostname_with_space_before_semicolon() {
  local resolv_conf="${TMP_DIR}/cube-proxy-hostname-space-resolv.conf"
  local global_conf="${TMP_DIR}/cube-proxy-global-space.conf"
  local resolver_inc="${TMP_DIR}/resolver-space.inc"

  cat > "${resolv_conf}" <<'EOF'
nameserver 169.254.254.53
EOF

  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com" ;
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>&1; then
    fail "prepare_resolver_include should accept global.conf redis_ip entries with whitespace before the semicolon"
  fi

  assert_contains "${resolver_inc}" "resolver 169.254.254.53 ipv6=off valid=30s;"
  assert_contains "${resolver_inc}" "resolver_timeout 5s;"
}

test_cube_proxy_start_always_writes_discovered_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-always-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-global-ip.conf"
  local resolver_inc="${TMP_DIR}/resolver-ip.inc"

  cat > "${resolv_conf}" <<'EOF'
nameserver 169.254.254.53
EOF

  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>&1; then
    fail "prepare_resolver_include should succeed when a resolver is available"
  fi

  assert_contains "${resolver_inc}" "resolver 169.254.254.53 ipv6=off valid=30s;"
}

test_cube_proxy_start_registry_hostname_fails_without_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-registry-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-registry-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-registry-none.inc"
  local err="${TMP_DIR}/cube-proxy-registry-no-resolver.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    CUBE_PROXY_REGISTRY_ENABLE=1 \
    CUBE_PROXY_REGISTRY_REDIS_HOST=registry.redis.example.com >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should fail when registry Redis needs DNS but no resolver is available"
  fi

  assert_contains "${err}" "CUBE_PROXY_REGISTRY_REDIS_HOST 'registry.redis.example.com' is not an IP literal"
}

test_cube_proxy_start_registry_hostname_uses_explicit_resolver_override() {
  local resolv_conf="${TMP_DIR}/cube-proxy-registry-explicit-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-registry-explicit-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-registry-explicit.inc"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    CUBE_PROXY_RESOLVER_ADDRS="172.18.0.10:5353 [2001:db8::53]:5353" \
    CUBE_PROXY_RESOLVER_VALID=300s \
    CUBE_PROXY_RESOLVER_TIMEOUT=10s \
    CUBE_PROXY_REGISTRY_ENABLE=1 \
    CUBE_PROXY_REGISTRY_REDIS_HOST=registry.redis.example.com >/dev/null 2>&1; then
    fail "prepare_resolver_include should honor the Helm resolver override for registry Redis"
  fi

  assert_contains "${resolver_inc}" "resolver 172.18.0.10:5353 [2001:db8::53]:5353 ipv6=off valid=300s;"
  assert_contains "${resolver_inc}" "resolver_timeout 10s;"
}

test_cube_proxy_start_hostname_uses_ipv6_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-ipv6-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-ipv6-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-ipv6.inc"

  cat > "${resolv_conf}" <<'EOF'
nameserver 2001:db8::53
EOF
  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>&1; then
    fail "prepare_resolver_include should support an IPv6-only resolver"
  fi

  assert_contains "${resolver_inc}" "resolver [2001:db8::53] ipv6=off valid=30s;"
}

test_cube_proxy_start_trailing_comment_hostname_fails_without_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-comment-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-comment-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-comment-none.inc"
  local err="${TMP_DIR}/cube-proxy-comment-no-resolver.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com"; # primary redis
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should not ignore a hostname followed by a trailing comment"
  fi

  assert_contains "${err}" "global.conf: \$redis_ip 'redis.example.com' is not an IP literal"
}

test_cube_proxy_start_uses_global_conf_over_unconsumed_env() {
  local resolv_conf="${TMP_DIR}/cube-proxy-env-mismatch-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-env-mismatch-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-env-mismatch-none.inc"
  local err="${TMP_DIR}/cube-proxy-env-mismatch-no-resolver.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com";
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    CUBE_PROXY_REDIS_IP=127.0.0.1 >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should use the Redis host nginx reads from global.conf"
  fi

  assert_contains "${err}" "global.conf: \$redis_ip 'redis.example.com' is not an IP literal"
}

test_cube_proxy_start_ip_targets_succeed_without_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-ip-targets-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-ip-targets-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-ip-targets-none.inc"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    CUBE_PROXY_REGISTRY_ENABLE=1 \
    CUBE_PROXY_REGISTRY_REDIS_HOST=10.0.0.12 >/dev/null 2>&1; then
    fail "prepare_resolver_include should allow IP-only Redis targets without a resolver"
  fi

  assert_contains "${resolver_inc}" "# no usable nginx resolver discovered"
}

test_cube_proxy_start_unparseable_redis_host_fails_without_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-unparseable-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-unparseable-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-unparseable-none.inc"
  local err="${TMP_DIR}/cube-proxy-unparseable-no-resolver.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip redis.example.com;
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should fail when it cannot classify the configured Redis target"
  fi

  assert_contains "${err}" "unable to parse global.conf: \$redis_ip"
}

test_cube_proxy_start_prepare_resolver_include_fails_without_resolver() {
  local resolv_conf="${TMP_DIR}/cube-proxy-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-global-no-resolver.conf"
  local resolver_inc="${TMP_DIR}/resolver-none.inc"
  local err="${TMP_DIR}/cube-proxy-no-resolver.err"

  cat > "${resolv_conf}" <<'EOF'
nameserver 2001:db8::zz
nameserver 999.999.999.999
EOF

  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com";
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should fail fast when Redis is a hostname and no valid resolver is available"
  fi

  assert_contains "${err}" "global.conf: \$redis_ip 'redis.example.com' is not an IP literal"
  assert_contains "${err}" "Ensure the container resolv.conf has at least one valid IPv4 or IPv6 nameserver."
}

test_cube_proxy_start_rejects_unspecified_ipv6_resolver() {
  local global_conf="${TMP_DIR}/cube-proxy-unspecified-ipv6-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-unspecified-ipv6.inc"
  local err="${TMP_DIR}/cube-proxy-unspecified-ipv6.err"

  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
EOF

  if run_prepare_resolver \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    CUBE_PROXY_RESOLVER_ADDRS="::" >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should reject the unspecified IPv6 resolver address"
  fi

  assert_contains "${err}" "invalid nameserver in CUBE_PROXY_RESOLVER_ADDRS: ::"
}

test_cube_proxy_start_uses_last_global_conf_redis_host() {
  local resolv_conf="${TMP_DIR}/cube-proxy-multiple-redis-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-multiple-redis-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-multiple-redis.inc"
  local err="${TMP_DIR}/cube-proxy-multiple-redis.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "127.0.0.1";
set $redis_ip "redis.example.com";
EOF

  if run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should classify the last global.conf redis_ip value"
  fi

  assert_contains "${err}" "global.conf: \$redis_ip 'redis.example.com' is not an IP literal"
}

test_cube_proxy_start_ignores_overridden_unparseable_redis_host() {
  local resolv_conf="${TMP_DIR}/cube-proxy-overridden-redis-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-overridden-redis-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-overridden-redis.inc"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip redis.ignored.example.com;
set $redis_ip "127.0.0.1";
EOF

  if ! run_prepare_resolver \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" >/dev/null 2>&1; then
    fail "prepare_resolver_include should classify only the last global.conf redis_ip declaration"
  fi

  assert_contains "${resolver_inc}" "# no usable nginx resolver discovered"
}

test_cube_proxy_start_main_exits_when_resolver_preparation_fails() {
  local resolv_conf="${TMP_DIR}/cube-proxy-main-no-resolver.conf"
  local global_conf="${TMP_DIR}/cube-proxy-main-global.conf"
  local resolver_inc="${TMP_DIR}/resolver-main-none.inc"
  local err="${TMP_DIR}/cube-proxy-main-no-resolver.err"

  : > "${resolv_conf}"
  cat > "${global_conf}" <<'EOF'
set $redis_ip "redis.example.com";
EOF

  if env \
    CUBE_PROXY_START_SH="${CUBE_PROXY_START_SH}" \
    CUBE_PROXY_RESOLV_CONF="${resolv_conf}" \
    CUBE_PROXY_GLOBAL_CONF_PATH="${global_conf}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    bash -c '
      set -euo pipefail
      source "${CUBE_PROXY_START_SH}"
      main
    ' >/dev/null 2>"${err}"; then
    fail "main should exit non-zero when resolver preparation fails"
  fi

  assert_contains "${err}" "global.conf: \$redis_ip 'redis.example.com' is not an IP literal"
  [[ ! -e "${resolver_inc}" ]] \
    || fail "main should not render a resolver include after preparation fails"
}

test_cube_proxy_start_discover_resolver_addresses_empty_and_missing() {
  local empty_resolv_conf="${TMP_DIR}/cube-proxy-empty-resolv.conf"
  local missing_resolv_conf="${TMP_DIR}/cube-proxy-missing-resolv.conf"
  local output=""

  : > "${empty_resolv_conf}"
  output="$(
    CUBE_PROXY_RESOLV_CONF="${empty_resolv_conf}" \
      bash -c '
        set -euo pipefail
        source "'"${CUBE_PROXY_START_SH}"'"
        discover_resolver_addresses
      '
  )"
  [[ -z "${output}" ]] \
    || fail "discover_resolver_addresses should return no addresses for an empty resolv.conf"

  output="$(
    CUBE_PROXY_RESOLV_CONF="${missing_resolv_conf}" \
      bash -c '
        set -euo pipefail
        source "'"${CUBE_PROXY_START_SH}"'"
        discover_resolver_addresses
      '
  )"
  [[ -z "${output}" ]] \
    || fail "discover_resolver_addresses should return no addresses for a missing resolv.conf"
}

test_cube_proxy_start_propagates_resolver_discovery_failure() {
  local resolver_inc="${TMP_DIR}/resolver-discovery-failure.inc"
  local err="${TMP_DIR}/resolver-discovery-failure.err"

  if env \
    CUBE_PROXY_START_SH="${CUBE_PROXY_START_SH}" \
    CUBE_PROXY_RESOLVER_INCLUDE_PATH="${resolver_inc}" \
    bash -c '
      set -euo pipefail
      source "${CUBE_PROXY_START_SH}"
      discover_resolver_addresses() {
        echo "simulated resolver discovery failure" >&2
        return 42
      }
      prepare_resolver_include
    ' >/dev/null 2>"${err}"; then
    fail "prepare_resolver_include should propagate resolver discovery failures"
  fi

  assert_contains "${err}" "simulated resolver discovery failure"
  [[ ! -e "${resolver_inc}" ]] \
    || fail "prepare_resolver_include should not render an include after resolver discovery fails"
}

test_cube_proxy_start_discover_resolver_addresses
test_cube_proxy_start_build_resolver_include
test_cube_proxy_start_prepare_resolver_include_for_hostname
test_cube_proxy_start_prepare_resolver_include_for_hostname_with_space_before_semicolon
test_cube_proxy_start_always_writes_discovered_resolver
test_cube_proxy_start_registry_hostname_fails_without_resolver
test_cube_proxy_start_registry_hostname_uses_explicit_resolver_override
test_cube_proxy_start_hostname_uses_ipv6_resolver
test_cube_proxy_start_trailing_comment_hostname_fails_without_resolver
test_cube_proxy_start_uses_global_conf_over_unconsumed_env
test_cube_proxy_start_ip_targets_succeed_without_resolver
test_cube_proxy_start_unparseable_redis_host_fails_without_resolver
test_cube_proxy_start_prepare_resolver_include_fails_without_resolver
test_cube_proxy_start_rejects_unspecified_ipv6_resolver
test_cube_proxy_start_uses_last_global_conf_redis_host
test_cube_proxy_start_ignores_overridden_unparseable_redis_host
test_cube_proxy_start_main_exits_when_resolver_preparation_fails
test_cube_proxy_start_discover_resolver_addresses_empty_and_missing
test_cube_proxy_start_propagates_resolver_discovery_failure

echo "CubeProxy resolver tests OK"
