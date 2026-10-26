#!/usr/bin/env bash
# CubeProxy admin access-log variable regression tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CUBE_PROXY_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
NGINX_CONF="${CUBE_PROXY_DIR}/nginx.conf"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"
}

assert_contains "${NGINX_CONF}" 'access_log /data/log/cube-proxy/access.log access;'
assert_contains "${NGINX_CONF}" "log_format access '\$access_time||\$host||"
assert_contains "${NGINX_CONF}" "\$http_x_cube_request_id||\$http_x_cube_instance_id||\$backend_ip';"
assert_contains "${CUBE_PROXY_DIR}/lua/log_phase.lua" 'ngx.var.access_time = get_currtime()'

admin_server="$({
  sed -n '/listen 127\.0\.0\.1:8082;/,/^    }/p' "${NGINX_CONF}"
})"
admin_server_prelocation="$({
  sed -n '/listen 127\.0\.0\.1:8082;/,/location \/admin\//p' "${NGINX_CONF}"
})"

grep -Fq 'set $access_time "";' <<<"${admin_server}" \
  || fail "8082 admin server must initialize access_time for the inherited access log"
grep -Fq 'set $backend_ip "";' <<<"${admin_server}" \
  || fail "8082 admin server must initialize backend_ip for the inherited access log"
grep -Fq 'set $ins_id "";' <<<"${admin_server}" \
  || fail "8082 admin server must initialize ins_id before reusing log_phase.lua"
grep -Fq 'log_by_lua_file lua/log_phase.lua;' <<<"${admin_server_prelocation}" \
  || fail "8082 server must populate access_time for every response through log_phase.lua"

if grep -Fq 'admin-access.log' "${NGINX_CONF}"; then
  fail "admin requests must keep using the shared access.log schema"
fi

echo "CubeProxy admin access-log tests passed"
