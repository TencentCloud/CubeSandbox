#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

export ONE_CLICK_TOOLBOX_ROOT="${TMP_DIR}/toolbox"
export ONE_CLICK_RUNTIME_DIR="${TMP_DIR}/run"
export ONE_CLICK_LOG_DIR="${TMP_DIR}/log"

# shellcheck source=../scripts/one-click/common.sh
source "${ONE_CLICK_DIR}/scripts/one-click/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_file() {
  [[ -f "$1" ]] || fail "expected file: $1"
}

assert_contains() {
  local path="$1"
  local needle="$2"
  rg -q --fixed-strings "${needle}" "${path}" || fail "expected ${path} to contain ${needle}"
}

assert_not_contains() {
  local path="$1"
  local needle="$2"
  if rg -q --fixed-strings "${needle}" "${path}"; then
    fail "expected ${path} not to contain ${needle}"
  fi
}

test_render_template_replaces_empty_directory() {
  local template="${TMP_DIR}/template.conf"
  local output="${TMP_DIR}/generated.conf"

  printf 'hello __NAME__\n' > "${template}"
  mkdir -p "${output}"

  render_template_atomic \
    "${template}" \
    "${output}" \
    -e "s/__NAME__/cube/g"

  assert_file "${output}"
  assert_contains "${output}" "hello cube"
}

test_render_template_rejects_non_empty_directory() {
  local template="${TMP_DIR}/template-non-empty.conf"
  local output="${TMP_DIR}/generated-non-empty.conf"

  printf 'hello\n' > "${template}"
  mkdir -p "${output}"
  printf 'keep\n' > "${output}/content"

  if (
    render_template_atomic \
      "${template}" \
      "${output}" \
      -e "s/hello/world/g"
  ) >/dev/null 2>&1; then
    fail "expected non-empty output directory to be rejected"
  fi
}

test_unit_prepare_hooks_are_wired() {
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-mysql.service" "ExecStartPre=/usr/local/services/cubetoolbox/scripts/systemd/mysql-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-redis.service" "ExecStartPre=/usr/local/services/cubetoolbox/scripts/systemd/redis-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-coredns.service" "ExecStartPre=/usr/local/services/cubetoolbox/scripts/systemd/coredns-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-coredns.service" "ExecStartPost=/usr/local/services/cubetoolbox/scripts/systemd/coredns-postcheck.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-proxy.service" "ExecStartPre=/usr/local/services/cubetoolbox/scripts/systemd/cube-proxy-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-webui.service" "ExecStartPre=/usr/local/services/cubetoolbox/scripts/systemd/webui-prepare.sh"
}

test_support_compose_render_is_locked_and_atomic() {
  local path="${ONE_CLICK_DIR}/scripts/one-click/up-support.sh"

  assert_contains "${path}" "require_cmd flock"
  assert_contains "${path}" "flock -x 9"
  assert_contains "${path}" "render_template_atomic"
  assert_not_contains "${path}" "> \"\${SUPPORT_COMPOSE_FILE}\""
}

test_compose_wrappers_reject_directories() {
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/compose-lib.sh" "ensure_bind_mount_file \"\${COMPOSE_FILE}\""
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/webui-compose-lib.sh" "ensure_bind_mount_file \"\${WEBUI_COMPOSE_FILE}\""
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/coredns-compose-lib.sh" "ensure_bind_mount_file \"\${COREDNS_COMPOSE_FILE}\""
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/support-compose-lib.sh" "ensure_bind_mount_file \"\${SUPPORT_COMPOSE_FILE}\""
}

test_coredns_direct_outputs_prepare_file_path() {
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/up-dns.sh" "prepare_file_output \"\${dst_path}\""
  assert_contains "${ONE_CLICK_DIR}/scripts/systemd/coredns-start.sh" "prepare_file_output \"\${dst_path}\""
  assert_contains "${ONE_CLICK_DIR}/scripts/systemd/common.sh" "wait_for_udp_port()"
  assert_contains "${ONE_CLICK_DIR}/scripts/systemd/common.sh" "require_cmd rg"
}

test_unit_dependency_order() {
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-proxy.service" "After=docker.service network-online.target cube-sandbox-redis.service cube-sandbox-dns.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cubemaster.service" "After=network-online.target cube-sandbox-mysql.service cube-sandbox-redis.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-api.service" "After=network-online.target cube-sandbox-cubemaster.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-webui.service" "After=docker.service network-online.target cube-sandbox-cube-api.service"
}

test_render_template_replaces_empty_directory
test_render_template_rejects_non_empty_directory
test_unit_prepare_hooks_are_wired
test_support_compose_render_is_locked_and_atomic
test_compose_wrappers_reject_directories
test_coredns_direct_outputs_prepare_file_path
test_unit_dependency_order

echo "runtime file safety tests OK"
