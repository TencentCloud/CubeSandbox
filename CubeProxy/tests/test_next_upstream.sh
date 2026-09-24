#!/usr/bin/env bash
# CubeProxy upstream retry-condition regression tests.
#
# balancer_phase.lua grants one extra try to every request, and the upstream
# blocks hold a single peer, so the *_next_upstream conditions decide what
# that try covers. They must stay limited to failures that leave no response
# header: error and timeout, never an http_* status, invalid_header or
# non_idempotent.

set -euo pipefail
shopt -s nullglob

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CUBE_PROXY_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
NGINX_CONF="${CUBE_PROXY_DIR}/nginx.conf"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# nginx.conf and the location snippets it includes. global.conf is rendered by
# the deployment templates outside CubeProxy/ and only sets variables.
conf_files=("${NGINX_CONF}" "${CUBE_PROXY_DIR}"/conf/includes/*.inc)

directives="$(grep -HE '^[[:space:]]*(proxy|grpc)_next_upstream[[:space:]]' "${conf_files[@]}" || true)"
while IFS= read -r entry; do
  [[ -n "${entry}" ]] || continue
  file="${entry%%:*}"
  directive="${entry#*:}"
  directive="${directive%%#*}"
  if grep -qwE 'http_[0-9]+|invalid_header|non_idempotent' <<<"${directive}"; then
    fail "${file}: retry conditions must not include a response status, invalid_header or non_idempotent: ${directive}"
  fi
  # off disables the retry for that context. Anything else must keep error
  # (the connect-reset retry depends on it) and timeout.
  if ! grep -qw 'off' <<<"${directive}"; then
    if ! grep -qw 'error' <<<"${directive}" || ! grep -qw 'timeout' <<<"${directive}"; then
      fail "${file}: retry conditions must include error and timeout: ${directive}"
    fi
  fi
done <<<"${directives}"

grep -qE '^[[:space:]]*proxy_next_upstream[[:space:]]' "${NGINX_CONF}" \
  || fail "nginx.conf must set proxy_next_upstream explicitly"

grep -Eq '^[[:space:]]*proxy_next_upstream_tries 2;' "${NGINX_CONF}" \
  || fail "proxy_next_upstream_tries must stay 2"
grep -Eq '^[[:space:]]*grpc_next_upstream_tries 2;' "${NGINX_CONF}" \
  || fail "grpc_next_upstream_tries must stay 2"

# The granted try must stay limited to the connect phase: 0 (or no directive)
# would let a request that failed late, after minutes, be replayed.
grep -Eq '^[[:space:]]*proxy_next_upstream_timeout[[:space:]]+[1-9][0-9]*(ms|s)?;' "${NGINX_CONF}" \
  || fail "nginx.conf must set a non-zero proxy_next_upstream_timeout"

echo "CubeProxy next-upstream tests passed"
