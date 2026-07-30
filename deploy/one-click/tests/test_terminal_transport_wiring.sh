#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

check_terminal_location() {
  local file="$1"
  local block
  block="$(sed -n '\|location = /opsapi/v1/terminal/ws {|,\|proxy_pass .*api/v1/terminal/ws;|p' "${file}")"
  [[ -n "${block}" ]] || fail "terminal WebSocket location missing from ${file}"
  local directive
  # The directive fixtures intentionally contain literal nginx variables.
  # shellcheck disable=SC2016
  for directive in \
    'proxy_http_version 1.1;' \
    'proxy_set_header Host $http_host;' \
    'proxy_set_header X-Real-IP $remote_addr;' \
    'proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;' \
    'proxy_set_header X-Forwarded-Proto $scheme;' \
    'proxy_set_header Upgrade $http_upgrade;' \
    'proxy_set_header Connection $connection_upgrade;' \
    'proxy_set_header Accept-Encoding "";' \
    'proxy_read_timeout 7200s;' \
    'proxy_send_timeout 7200s;' \
    'proxy_buffering off;' \
    'proxy_request_buffering off;' \
    'gzip off;'; do
    grep -q -F "${directive}" <<<"${block}" \
      || fail "${file} terminal location missing: ${directive}"
  done
  # shellcheck disable=SC2016
  if grep -q -F 'proxy_set_header Host $host;' <<<"${block}"; then
    fail "${file} terminal location strips a non-default browser Origin port"
  fi
}

check_terminal_location "${ONE_CLICK_DIR}/webui/nginx.conf"
check_terminal_location "${ROOT_DIR}/deploy/kubernetes/chart/templates/_helpers.tpl"
check_terminal_location "${ONE_CLICK_DIR}/terraform/tencentcloud/tke-addons.tf"
check_terminal_location "${ONE_CLICK_DIR}/terraform/tencentcloud/create.sh"

grep -A14 -F 'location /opsapi/ {' "${ONE_CLICK_DIR}/webui/nginx.conf" \
  | grep -q -F 'proxy_read_timeout 300s;' \
  || fail "ordinary one-click /opsapi REST timeout changed"
grep -A14 -F 'location /opsapi/ {' "${ROOT_DIR}/deploy/kubernetes/chart/templates/_helpers.tpl" \
  | grep -q -F 'proxy_read_timeout 300s;' \
  || fail "ordinary Helm /opsapi REST timeout changed"
grep -A5 -F "'/opsapi': {" "${ROOT_DIR}/web/vite.config.ts" \
  | grep -q -F 'ws: true,' \
  || fail "Vite /opsapi proxy is not WebSocket-enabled"

while IFS='|' read -r key value; do
  grep -q -x -F "${key}=${value}" "${ONE_CLICK_DIR}/env.example" \
    || fail "one-click env default ${key}=${value} is missing"
  expected="export ${key}=\"\${${key}:-${value}}\""
  grep -q -F "${expected}" "${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh" \
    || fail "cubeops-start.sh default for ${key} does not match env.example"
done <<'DEFAULTS'
CUBE_TERMINAL_ENABLED|true
CUBE_TERMINAL_ALLOWED_ORIGINS|
CUBE_TERMINAL_GRANT_TTL_SECONDS|60
CUBE_TERMINAL_HANDSHAKE_TIMEOUT_SECONDS|10
CUBE_TERMINAL_PING_INTERVAL_SECONDS|20
CUBE_TERMINAL_PONG_TIMEOUT_SECONDS|10
CUBE_TERMINAL_WRITE_DEADLINE_SECONDS|10
CUBE_TERMINAL_IDLE_TIMEOUT_MINUTES|30
CUBE_TERMINAL_MAX_LIFETIME_HOURS|8
CUBE_TERMINAL_RECONNECT_GRACE_SECONDS|30
CUBE_TERMINAL_REPLAY_BUFFER_BYTES|262144
CUBE_TERMINAL_MAX_FRAME_BYTES|65536
CUBE_TERMINAL_STDIN_QUEUE_FRAMES|8
CUBE_TERMINAL_STDOUT_PENDING_BYTES|262144
CUBE_TERMINAL_MAX_SESSIONS_PER_USER|5
CUBE_TERMINAL_MAX_SESSIONS_PER_REPLICA|200
CUBE_TERMINAL_DRAIN_TIMEOUT_SECONDS|30
DEFAULTS

echo "terminal transport wiring tests OK"
