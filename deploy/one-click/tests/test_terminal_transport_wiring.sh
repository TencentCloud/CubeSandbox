#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
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

echo "terminal transport wiring tests OK"
