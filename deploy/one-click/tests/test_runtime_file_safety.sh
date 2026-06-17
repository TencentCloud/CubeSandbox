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

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"
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
  grep -Fq -- "${needle}" "${path}" || fail "expected ${path} to contain ${needle}"
}

assert_not_contains() {
  local path="$1"
  local needle="$2"
  if grep -Fq -- "${needle}" "${path}"; then
    fail "expected ${path} not to contain ${needle}"
  fi
}

run_cube_proxy_postcheck_case() {
  local env_content="$1"
  local listen_port="$2"
  local expected_port="$3"
  local case_dir="${TMP_DIR}/cube-proxy-postcheck-${expected_port}"
  local env_file="${case_dir}/.one-click.env"
  local stub_dir="${case_dir}/bin"
  local ss_log="${case_dir}/ss.args"

  mkdir -p "${stub_dir}"
  printf '%s\n' "${env_content}" > "${env_file}"
  cat > "${stub_dir}/ss" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${SS_ARGS_LOG}"
printf 'LISTEN 0 128 0.0.0.0:%s 0.0.0.0:*\n' "${SS_LISTEN_PORT}"
SH
  chmod +x "${stub_dir}/ss"

  PATH="${stub_dir}:${PATH}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${env_file}" \
    SS_ARGS_LOG="${ss_log}" \
    SS_LISTEN_PORT="${listen_port}" \
    bash "${ONE_CLICK_DIR}/scripts/systemd/cube-proxy-postcheck.sh" >/dev/null

  assert_contains "${ss_log}" "sport = :${expected_port}"
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
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-mysql.service" "/usr/local/services/cubetoolbox/scripts/systemd/mysql-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-redis.service" "/usr/local/services/cubetoolbox/scripts/systemd/redis-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-coredns.service" "/usr/local/services/cubetoolbox/scripts/systemd/coredns-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-coredns.service" "/usr/local/services/cubetoolbox/scripts/systemd/coredns-postcheck.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-proxy.service" "/usr/local/services/cubetoolbox/scripts/systemd/cube-proxy-prepare.sh"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-webui.service" "/usr/local/services/cubetoolbox/scripts/systemd/webui-prepare.sh"
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
  assert_not_contains "${ONE_CLICK_DIR}/scripts/systemd/common.sh" "require_cmd rg"
}

test_unit_dependency_order() {
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-proxy.service" "After=docker.service network-online.target cube-sandbox-redis.service cube-sandbox-dns.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cubemaster.service" "After=network-online.target cube-sandbox-mysql.service cube-sandbox-redis.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-api.service" "After=network-online.target cube-sandbox-cubemaster.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-webui.service" "After=docker.service network-online.target cube-sandbox-cube-api.service"
}

test_detect_glibc_version_consumes_full_ldd_output() {
  ldd() {
    printf 'ldd (GNU libc) 2.39\n'
    seq 1 100000
  }

  local version
  version="$(detect_glibc_version)" || fail "expected detect_glibc_version to succeed with long ldd output"
  [[ "${version}" == "2.39" ]] || fail "expected glibc version 2.39, got ${version}"
}

test_online_install_glibc_detection_avoids_head_pipe() {
  local path="${ONE_CLICK_DIR}/online-install.sh"

  assert_contains "${path}" "detect_glibc_version()"
  assert_contains "${path}" "ldd_output=\"\$(ldd --version 2>&1)\""
  assert_not_contains "${path}" "ldd --version 2>&1 | head -1 | awk '{print \$NF}'"
}

test_one_click_scripts_do_not_require_ripgrep() {
  assert_not_contains "${ONE_CLICK_DIR}/install.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/install.sh" "install_ripgrep"
  assert_not_contains "${ONE_CLICK_DIR}/lib/common.sh" "install_ripgrep"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/one-click/common.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/systemd/common.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/one-click/up-with-deps.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/one-click/down-with-deps.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/one-click/up-webui.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/one-click/up-compute.sh" "require_cmd rg"
  assert_not_contains "${ONE_CLICK_DIR}/scripts/systemd/prepare-compute-role.sh" "require_cmd rg"
}

test_quickcheck_reports_node_registration_failure_explicitly() {
  local path="${ONE_CLICK_DIR}/scripts/one-click/quickcheck.sh"

  assert_contains "${path}" "failed to query cubemaster node registration"
  assert_contains "${path}" "cubemaster node registration missing host_ip="
  assert_not_contains "${path}" "| rg -q"
}

test_cube_proxy_postcheck_defaults_to_http_port_80() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    80 \
    80
}

test_cube_proxy_postcheck_follows_http_port() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_HTTP_PORT=8081
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    8081 \
    8081
}

test_cube_proxy_postcheck_ignores_https_port() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_HTTPS_PORT=8843
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    80 \
    80
}

test_cube_proxy_postcheck_ignores_deprecated_host_port() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_HOST_PORT=9443
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    80 \
    80
}

test_cube_proxy_postcheck_prefers_http_over_deprecated_host_port() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_HTTP_PORT=8081
CUBE_PROXY_HOST_PORT=9443
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    8081 \
    8081
}

test_render_template_replaces_empty_directory
test_render_template_rejects_non_empty_directory
test_unit_prepare_hooks_are_wired
test_support_compose_render_is_locked_and_atomic
test_compose_wrappers_reject_directories
test_coredns_direct_outputs_prepare_file_path
test_unit_dependency_order
test_detect_glibc_version_consumes_full_ldd_output
test_online_install_glibc_detection_avoids_head_pipe
test_one_click_scripts_do_not_require_ripgrep
test_quickcheck_reports_node_registration_failure_explicitly
test_cube_proxy_postcheck_defaults_to_http_port_80
test_cube_proxy_postcheck_follows_http_port
test_cube_proxy_postcheck_ignores_https_port
test_cube_proxy_postcheck_ignores_deprecated_host_port
test_cube_proxy_postcheck_prefers_http_over_deprecated_host_port

echo "runtime file safety tests OK"
