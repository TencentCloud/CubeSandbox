#!/usr/bin/env bash
# Several behavioral tests below source one-click scripts and drive their helper
# functions indirectly (dispatched via `"$@"`) inside subshells. shellcheck
# cannot follow the dynamic `source` or the indirect dispatch, so it reports the
# stub bodies as unreachable (SC2317), the budget variables as unused (SC2034),
# and the in-subshell assignments as lost (SC2030/SC2031) even though the reads
# happen in the same subshell. These are false positives for this test pattern.
# shellcheck disable=SC2030,SC2031,SC2034,SC2317
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

: > "${TMP_DIR}/empty-runtime.env"
export ONE_CLICK_RUNTIME_ENV_FILE="${TMP_DIR}/empty-runtime.env"
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

assert_stdout_contains() {
  local haystack="$1"
  local needle="$2"
  case "${haystack}" in
    *"${needle}"*) : ;;
    *) fail "expected output to contain '${needle}', got: ${haystack}" ;;
  esac
}

run_cube_proxy_postcheck_case() {
  local env_content="$1"
  local listen_port="$2"
  local expected_port="$3"
  local grpc_listen_port="${4:-9090}"
  local grpc_expected_port="${5:-9090}"
  local admin_port="${6:-8082}"
  local case_dir="${TMP_DIR}/cube-proxy-postcheck-${expected_port}-${grpc_expected_port}-${admin_port}"
  local env_file="${case_dir}/.one-click.env"
  local stub_dir="${case_dir}/bin"
  local ss_log="${case_dir}/ss.args"

  mkdir -p "${stub_dir}"
  printf '%s\n' "${env_content}" > "${env_file}"
  # The postcheck probes HTTP + gRPC + admin, so the ss stub is invoked three
  # times and must APPEND (>>) -- not overwrite (>) -- each call's args.
  cat > "${stub_dir}/ss" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${SS_ARGS_LOG}"
port=""
for arg in "$@"; do
  case "${arg}" in
    *"sport = :"*) port="${arg#*sport = :}"; port="${port%% )*}" ;;
  esac
done
if [[ "${port}" == "${SS_HTTP_PORT}" || "${port}" == "${SS_GRPC_PORT}" || "${port}" == "${SS_ADMIN_PORT}" ]]; then
  printf 'LISTEN 0 128 0.0.0.0:%s 0.0.0.0:*\n' "${port}"
fi
SH
  chmod +x "${stub_dir}/ss"

  PATH="${stub_dir}:${PATH}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${env_file}" \
    SS_ARGS_LOG="${ss_log}" \
    SS_HTTP_PORT="${listen_port}" \
    SS_GRPC_PORT="${grpc_listen_port}" \
    SS_ADMIN_PORT="${admin_port}" \
    bash "${ONE_CLICK_DIR}/scripts/systemd/cube-proxy-postcheck.sh" >/dev/null

  assert_contains "${ss_log}" "sport = :${expected_port}"
  assert_contains "${ss_log}" "sport = :${grpc_expected_port}"
  assert_contains "${ss_log}" "sport = :${admin_port}"
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
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-minio.service" "/usr/local/services/cubetoolbox/scripts/systemd/minio-prepare.sh"
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
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-proxy.service" "After=docker.service network-online.target cube-sandbox-redis.service cube-sandbox-dns.service cube-sandbox-cube-lifecycle-manager.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cubemaster.service" "After=network-online.target cube-sandbox-mysql.service cube-sandbox-redis.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-cube-api.service" "After=network-online.target cube-sandbox-cubemaster.service"
  assert_contains "${ONE_CLICK_DIR}/systemd/cube-sandbox-webui.service" "After=docker.service network-online.target cube-sandbox-cubemaster.service cube-sandbox-cubeops.service"
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

  assert_contains "${path}" "failed to query CubeOps node registration for "
  assert_contains "${path}" "CubeOps node registration missing IP="
  assert_not_contains "${path}" "| rg -q"
}

# quickcheck runs once right after `systemctl enable --now <target>` returns, so
# it must tolerate the brief startup race before cubelet binds
# their sockets and serve their health endpoints. Guard the retry semantics so a
# future refactor cannot reintroduce one-shot probes (which produced spurious
# "compute installation failed ... exit 1" failures on healthy nodes).
test_quickcheck_probes_are_race_tolerant() {
  local path="${ONE_CLICK_DIR}/scripts/one-click/quickcheck.sh"

  assert_contains "${path}" "CUBE_QUICKCHECK_READY_TIMEOUT"
  assert_contains "${path}" "wait_until"
  # Socket / health probes must go through the retrying helpers, never a bare
  # one-shot check that dies on the first transient miss.
  assert_contains "${path}" "check_socket "
  assert_contains "${path}" "check_http "
  assert_not_contains "${path}" 'curl -fsS "http://${NA_HEALTH_ADDR}/healthz" >/dev/null'
  assert_not_contains "${path}" 'test -S "/data/cubelet/cubelet.sock"'
}

# Source quickcheck.sh for its helper functions (its probe sequence is guarded
# behind a BASH_SOURCE check) so the retry semantics can be exercised at
# runtime, not just asserted as source text. `sleep` is stubbed to a no-op so
# the retry loops run instantly.
_quickcheck_source() {
  # shellcheck source=../scripts/one-click/quickcheck.sh
  source "${ONE_CLICK_DIR}/scripts/one-click/quickcheck.sh"
}

test_quickcheck_wait_until_retries_then_succeeds() {
  local counter_file="${TMP_DIR}/wait-until-attempts"
  printf '0\n' > "${counter_file}"
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    flaky() {
      local n
      n="$(<"${counter_file}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${counter_file}"
      (( n >= 3 ))
    }
    wait_until "flaky predicate never ready" flaky
  ) || fail "wait_until should succeed once the predicate passes"
  local attempts
  attempts="$(<"${counter_file}")"
  (( attempts == 3 )) || fail "wait_until should retry until success (expected 3 attempts, got ${attempts})"
}

test_quickcheck_wait_until_times_out_with_descriptive_die() {
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=4
    QUICKCHECK_READY_INTERVAL=2
    # Deadline already in the past -> fail on the first miss without hanging.
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    wait_until "widget not ready" false
  )"; then
    fail "wait_until should die when the predicate never passes"
  fi
  assert_stdout_contains "${out}" "widget not ready (not ready within 4s)"
}

test_quickcheck_timeout_zero_is_fail_fast() {
  # With timeout=0 wait_until must probe exactly once and die WITHOUT sleeping.
  # All three guards (no sleep, descriptive die, single probe) are asserted in
  # the parent shell so a regression of the `>=` deadline boundary is caught --
  # an in-subshell `fail` inside the dying call would be swallowed by die's exit.
  local counter_file="${TMP_DIR}/fail-fast-attempts"
  local sleep_log="${TMP_DIR}/fail-fast-slept"
  printf '0\n' > "${counter_file}"
  : > "${sleep_log}"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT=0
    unset CUBE_QUICKCHECK_READY_INTERVAL || true
    quickcheck_init_budget
    sleep() { echo slept >> "${sleep_log}"; }
    always_fail() {
      local n
      n="$(<"${counter_file}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${counter_file}"
      return 1
    }
    wait_until "x not ready" always_fail
  )"; then
    fail "wait_until must fail-fast (die) with timeout=0"
  fi
  # The "within 0s" message confirms the budget normalized to 0.
  assert_stdout_contains "${out}" "x not ready (not ready within 0s)"
  [[ ! -s "${sleep_log}" ]] || fail "timeout=0 must not sleep before failing"
  local attempts
  attempts="$(<"${counter_file}")"
  (( attempts == 1 )) || fail "timeout=0 must probe exactly once, got ${attempts}"
}

test_quickcheck_invalid_budget_falls_back_to_defaults() {
  (
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT="abc"
    CUBE_QUICKCHECK_READY_INTERVAL="0"
    quickcheck_init_budget 2>/dev/null
    (( QUICKCHECK_READY_TIMEOUT == 120 )) || fail "invalid timeout should fall back to 120, got ${QUICKCHECK_READY_TIMEOUT}"
    (( QUICKCHECK_READY_INTERVAL == 2 )) || fail "invalid/zero interval should fall back to 2, got ${QUICKCHECK_READY_INTERVAL}"
  ) || fail "invalid budget fallback test failed"
}

test_quickcheck_budget_normalizes_leading_zeros() {
  # Leading-zero values must be parsed as base 10, not octal (which would abort
  # the arithmetic under set -e for digits 8/9).
  (
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT="0090"
    CUBE_QUICKCHECK_READY_INTERVAL="08"
    quickcheck_init_budget
    (( QUICKCHECK_READY_TIMEOUT == 90 )) || fail "leading-zero timeout should normalize to 90, got ${QUICKCHECK_READY_TIMEOUT}"
    (( QUICKCHECK_READY_INTERVAL == 8 )) || fail "leading-zero interval should normalize to 8, got ${QUICKCHECK_READY_INTERVAL}"
  ) || fail "leading-zero normalization test failed"
}

test_quickcheck_interval_clamped_to_timeout() {
  (
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT=3
    CUBE_QUICKCHECK_READY_INTERVAL=10
    quickcheck_init_budget
    (( QUICKCHECK_READY_INTERVAL == 3 )) || fail "interval should clamp to timeout (3), got ${QUICKCHECK_READY_INTERVAL}"
  ) || fail "interval clamp test failed"
}

test_quickcheck_timeout_clamped_to_max() {
  (
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT=99999
    unset CUBE_QUICKCHECK_READY_INTERVAL || true
    quickcheck_init_budget 2>/dev/null
    (( QUICKCHECK_READY_TIMEOUT == 3600 )) || fail "timeout should clamp to 3600, got ${QUICKCHECK_READY_TIMEOUT}"
  ) || fail "timeout max clamp test failed"
}

test_quickcheck_timeout_overflow_falls_back_to_default() {
  # An 8+ digit value fails the ^[0-9]{1,7}$ guard entirely, so it must fall back
  # to the default (120) -- NOT be clamped to the 3600 max, which only applies to
  # in-range values. This distinguishes the overflow-reject branch from the
  # max-clamp branch.
  (
    _quickcheck_source
    CUBE_QUICKCHECK_READY_TIMEOUT=12345678
    unset CUBE_QUICKCHECK_READY_INTERVAL || true
    quickcheck_init_budget 2>/dev/null
    (( QUICKCHECK_READY_TIMEOUT == 120 )) || fail "overflow timeout should fall back to 120, got ${QUICKCHECK_READY_TIMEOUT}"
  ) || fail "timeout overflow fallback test failed"
}

test_quickcheck_check_executable_behaviour() {
  local exe="${TMP_DIR}/qc-exe"
  printf '#!/bin/sh\n' > "${exe}"
  chmod +x "${exe}"
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=5
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 5 ))
    check_executable "${exe}"
  ) || fail "check_executable should pass for an executable file"

  local missing="${TMP_DIR}/qc-missing-exe"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=2
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    check_executable "${missing}"
  )"; then
    fail "check_executable should die for a missing executable"
  fi
  assert_stdout_contains "${out}" "expected executable not ready: ${missing}"
}

test_quickcheck_bind_mount_file_uses_specific_message() {
  # check_bind_mount_source_file must keep its specific message rather than the
  # generic check_file default.
  local missing="${TMP_DIR}/qc-missing-bind"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=2
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    check_bind_mount_source_file "${missing}"
  )"; then
    fail "check_bind_mount_source_file should die for a missing file"
  fi
  assert_stdout_contains "${out}" "expected bind mount source file not ready: ${missing}"
}

test_quickcheck_node_registration_keeps_missing_ip_after_blip() {
  # Once CubeOps has been reached (registration present but IP wrong), a
  # later connectivity blip must NOT downgrade the diagnostic reason back to
  # "failed to query". Drive the wall clock deterministically with a stubbed
  # date so the deadline is crossed exactly on the second iteration.
  local curl_counter="${TMP_DIR}/reg-curl-calls"
  local date_counter="${TMP_DIR}/reg-date-calls"
  printf '0\n' > "${curl_counter}"
  printf '0\n' > "${date_counter}"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    MASTER_ADDR="127.0.0.1:8089"
    QUICKCHECK_READY_TIMEOUT=10
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=101
    date() {
      local n
      n="$(<"${date_counter}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${date_counter}"
      printf '%s\n' "$(( 99 + n ))"
    }
    curl() {
      local n
      n="$(<"${curl_counter}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${curl_counter}"
      if (( n == 1 )); then
        # Iteration 1: reachable CubeOps, wrong IP -> reached=1.
        printf '{"IP":"10.0.0.99"}\n'
        return 0
      fi
      # Iteration 2: connectivity blip.
      return 7
    }
    check_node_registration "10.0.0.10" "${MASTER_ADDR}"
  )"; then
    fail "check_node_registration should die when IP never matches"
  fi
  assert_stdout_contains "${out}" "CubeOps node registration missing IP=10.0.0.10"
  case "${out}" in
    *"failed to query CubeOps node registration for 10.0.0.10"*)
      fail "reason must stay sticky as 'missing IP' after CubeOps was reached" ;;
  esac
}

test_quickcheck_container_ready_retries_transient_states() {
  # Every transient startup status (empty/created/restarting/starting) must be
  # retried (not an immediate die) until the container reports running. Covering
  # each one guards the transient arm of the case statement against a refactor
  # that moves one into the terminal `*)` branch.
  local docker_calls="${TMP_DIR}/container-docker-calls"
  printf '0\n' > "${docker_calls}"
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    docker() {
      local n
      n="$(<"${docker_calls}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${docker_calls}"
      case "${n}" in
        1) printf '\n' ;;           # not created yet -> empty status
        2) printf 'created\n' ;;
        3) printf 'restarting\n' ;;
        4) printf 'starting\n' ;;
        *) printf 'running\n' ;;
      esac
    }
    check_container_ready "cube-sandbox-mysql"
  ) || fail "check_container_ready should retry transient states then succeed"
  local calls
  calls="$(<"${docker_calls}")"
  (( calls == 5 )) || fail "expected 5 docker inspect calls, got ${calls}"
}

test_quickcheck_container_ready_dies_on_terminal_status() {
  # A terminal status (e.g. exited) must die immediately without retrying.
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { fail "terminal status must not be retried"; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    docker() { printf 'exited\n'; }
    check_container_ready "cube-sandbox-mysql"
  )"; then
    fail "check_container_ready should die on a terminal container status"
  fi
  assert_stdout_contains "${out}" "container is not ready: cube-sandbox-mysql (status=exited)"
}

test_quickcheck_container_ready_bounded_by_overall_deadline() {
  # Even with a huge per-container budget, the shared overall deadline caps the
  # wait and the failure is attributed to the overall budget.
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    CUBE_QUICKCHECK_CONTAINER_TIMEOUT=9999
    QUICKCHECK_READY_TIMEOUT=5
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    docker() { printf 'starting\n'; }
    check_container_ready "cube-sandbox-mysql"
  )"; then
    fail "check_container_ready should die when the overall deadline has passed"
  fi
  assert_stdout_contains "${out}" "overall quickcheck budget"
}

test_quickcheck_container_ready_dies_on_per_container_timeout() {
  # A small CUBE_QUICKCHECK_CONTAINER_TIMEOUT must fail a wedged-but-not-terminal
  # container sooner than (and attributed to) the overall budget. Drive the wall
  # clock with a stubbed date so the per-container budget trips before the much
  # larger overall deadline.
  local date_counter="${TMP_DIR}/per-container-date-calls"
  printf '0\n' > "${date_counter}"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    CUBE_QUICKCHECK_CONTAINER_TIMEOUT=2
    quickcheck_init_budget
    QUICKCHECK_READY_INTERVAL=2
    # Overall deadline far in the future so only the per-container budget can trip.
    date() {
      local n
      n="$(<"${date_counter}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${date_counter}"
      printf '%s\n' "$(( 1000 + n ))"
    }
    QUICKCHECK_DEADLINE=999999
    docker() { printf 'starting\n'; }
    check_container_ready "cube-sandbox-mysql"
  )"; then
    fail "check_container_ready should die when the per-container timeout is exhausted"
  fi
  assert_stdout_contains "${out}" "container is not ready within 2s: cube-sandbox-mysql (status=starting)"
  case "${out}" in
    *"overall quickcheck budget"*)
      fail "per-container timeout failure must not be attributed to the overall budget" ;;
  esac
}

test_quickcheck_container_ready_accepts_healthy_status() {
  # A docker healthcheck reporting "healthy" must be accepted, not just "running".
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    docker() { printf 'healthy\n'; }
    check_container_ready "cube-sandbox-mysql"
  ) || fail "check_container_ready should accept a healthy container"
}

test_quickcheck_budget_is_shared_across_sequential_probes() {
  # The headline invariant: QUICKCHECK_DEADLINE is computed once and is NOT reset
  # per probe, so a later probe runs against the same (already-elapsing) deadline
  # rather than getting a fresh window. A stubbed clock advances 1s per call;
  # wait_until is invoked WITHOUT recomputing the deadline between calls, exactly
  # as quickcheck_main does. The second probe must die once the shared deadline
  # passes -- a per-probe reset would let it loop forever against this stub.
  local date_counter="${TMP_DIR}/shared-budget-date-calls"
  printf '0\n' > "${date_counter}"
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=3
    QUICKCHECK_READY_INTERVAL=1
    date() {
      local n
      n="$(<"${date_counter}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${date_counter}"
      printf '%s\n' "${n}"
    }
    # Shared deadline at "second" 3. Re-running quickcheck_init_budget here would
    # move it; the production code does not, and neither does this test.
    QUICKCHECK_DEADLINE=3
    # An earlier probe that succeeds, then a later probe that never does. The
    # later probe inherits the same deadline and must terminate.
    wait_until "first probe" true
    wait_until "second probe" false
  )"; then
    fail "second probe must die against the shared (already-elapsing) deadline"
  fi
  assert_stdout_contains "${out}" "second probe (not ready"
}

test_quickcheck_callers_do_not_wrap_in_retry_loop() {
  # quickcheck self-retries now; up.sh / up-compute.sh must call it exactly once
  # and must not re-introduce the old outer retry loop.
  local up="${ONE_CLICK_DIR}/scripts/one-click/up.sh"
  local upc="${ONE_CLICK_DIR}/scripts/one-click/up-compute.sh"
  assert_contains "${up}" '"${SCRIPT_DIR}/quickcheck.sh"'
  assert_contains "${upc}" '"${SCRIPT_DIR}/quickcheck.sh"'
  assert_not_contains "${up}" 'for _ in {1..30}'
  assert_not_contains "${upc}" 'for _ in {1..30}'
}

test_quickcheck_node_registration_prefers_missing_ip_reason() {
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    MASTER_ADDR="127.0.0.1:8089"
    QUICKCHECK_READY_TIMEOUT=4
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    # Reachable CubeOps, but the registered IP never matches.
    curl() { printf '{"IP":"10.0.0.99"}\n'; }
    check_node_registration "10.0.0.10" "${MASTER_ADDR}"
  )"; then
    fail "check_node_registration should die when IP never matches"
  fi
  assert_stdout_contains "${out}" "CubeOps node registration missing IP=10.0.0.10"
}

test_quickcheck_node_registration_reports_unreachable_reason() {
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    MASTER_ADDR="127.0.0.1:8089"
    QUICKCHECK_READY_TIMEOUT=4
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    # Simulate a refused connection (curl exit 7).
    curl() { return 7; }
    check_node_registration "10.0.0.10" "${MASTER_ADDR}"
  )"; then
    fail "check_node_registration should die when CubeOps is unreachable"
  fi
  assert_stdout_contains "${out}" "failed to query CubeOps node registration for 10.0.0.10"
}

test_quickcheck_node_registration_succeeds_on_first_match() {
  (
    _quickcheck_source
    sleep() { :; }
    MASTER_ADDR="127.0.0.1:8089"
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    curl() { printf '{"IP":"10.0.0.10"}\n'; }
    check_node_registration "10.0.0.10" "${MASTER_ADDR}"
  ) || fail "check_node_registration should succeed when IP matches on the first attempt"
}

test_quickcheck_node_registration_response_missing_ip_field() {
  # When curl returns valid JSON without an IP field, the reason must
  # distinguish "unrecognized response" from "known format, wrong IP".
  local out
  if out="$(
    exec 2>&1
    _quickcheck_source
    sleep() { :; }
    MASTER_ADDR="127.0.0.1:8089"
    QUICKCHECK_READY_TIMEOUT=4
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=$(( $(date +%s) - 1 ))
    curl() { printf '{"error":"not ready"}\n'; }
    check_node_registration "10.0.0.10" "${MASTER_ADDR}"
  )"; then
    fail "check_node_registration should die when response lacks IP field"
  fi
  assert_stdout_contains "${out}" "CubeOps node registration response missing IP field for 10.0.0.10"
}

test_quickcheck_check_socket_retries_then_succeeds() {
  local test_counter="${TMP_DIR}/socket-test-calls"
  printf '0\n' > "${test_counter}"
  local sock="${TMP_DIR}/qc-test-sock"
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    test() {
      if [[ "$1" == "-S" ]]; then
        local n
        n="$(<"${test_counter}")"
        n=$(( n + 1 ))
        printf '%s\n' "${n}" > "${test_counter}"
        (( n >= 3 ))
      else
        command test "$@"
      fi
    }
    check_socket "${sock}"
  ) || fail "check_socket should retry then succeed once the socket appears"
  local calls
  calls="$(<"${test_counter}")"
  (( calls == 3 )) || fail "check_socket should retry 3 times, got ${calls}"
}

test_quickcheck_check_http_retries_then_succeeds() {
  local curl_counter="${TMP_DIR}/http-test-calls"
  printf '0\n' > "${curl_counter}"
  (
    _quickcheck_source
    sleep() { :; }
    QUICKCHECK_READY_TIMEOUT=30
    QUICKCHECK_READY_INTERVAL=1
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 30 ))
    curl() {
      local n
      n="$(<"${curl_counter}")"
      n=$(( n + 1 ))
      printf '%s\n' "${n}" > "${curl_counter}"
      (( n >= 3 ))
    }
    check_http "http://127.0.0.1:9998/v1/policies/dump"
  ) || fail "check_http should retry then succeed once the endpoint responds"
  local calls
  calls="$(<"${curl_counter}")"
  (( calls == 3 )) || fail "check_http should retry 3 times, got ${calls}"
}

# A unit that has already failed (e.g. cube-proxy nginx aborting on a bind
# conflict) will never become active. check_unit_active must die immediately
# instead of polling until QUICKCHECK_DEADLINE.
test_quickcheck_check_unit_active_dies_fast_on_failed_unit() {
  local sleep_log="${TMP_DIR}/failed-unit-slept"
  local out_log="${TMP_DIR}/failed-unit-out"
  : > "${sleep_log}"
  : > "${out_log}"
  (
    exec >"${out_log}" 2>&1
    _quickcheck_source
    QUICKCHECK_READY_TIMEOUT=120
    QUICKCHECK_READY_INTERVAL=2
    QUICKCHECK_DEADLINE=$(( $(date +%s) + 120 ))
    sleep() { echo slept >> "${sleep_log}"; }
    systemctl() {
      case "$1" in
        show) printf 'failed\n' ;;
        status) echo "status stub"; return 0 ;;
      esac
    }
    check_unit_active cube-sandbox-cube-proxy.service
  ) && fail "check_unit_active should die fast when the unit has failed" || true
  local out
  out="$(<"${out_log}")"
  assert_stdout_contains "${out}" "systemd unit failed: cube-sandbox-cube-proxy.service"
  assert_stdout_contains "${out}" "status stub"
  [[ ! -s "${sleep_log}" ]] || fail "check_unit_active must not sleep before failing on a failed (terminal) unit"
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

test_cube_proxy_postcheck_follows_grpc_port() {
  run_cube_proxy_postcheck_case \
    "CUBE_PROXY_HTTP_PORT=8081
CUBE_PROXY_GRPC_PORT=50051
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" \
    8081 \
    8081 \
    50051 \
    50051
}

test_cube_proxy_postcheck_fails_when_grpc_port_not_ready() {
  local case_dir="${TMP_DIR}/cube-proxy-postcheck-grpc-fail"
  local env_file="${case_dir}/.one-click.env"
  local stub_dir="${case_dir}/bin"
  local ss_log="${case_dir}/ss.args"

  mkdir -p "${stub_dir}"
  printf '%s\n' \
    "CUBE_PROXY_HTTP_PORT=80
CUBE_PROXY_GRPC_PORT=9090
CUBE_PROXY_POSTCHECK_RETRIES=1
CUBE_PROXY_POSTCHECK_DELAY=0" > "${env_file}"
  cat > "${stub_dir}/ss" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${SS_ARGS_LOG}"
port=""
for arg in "$@"; do
  case "${arg}" in
    *"sport = :"*) port="${arg#*sport = :}"; port="${port%% )*}" ;;
  esac
done
if [[ "${port}" == "${SS_HTTP_PORT}" ]]; then
  printf 'LISTEN 0 128 0.0.0.0:%s 0.0.0.0:*\n' "${port}"
fi
SH
  chmod +x "${stub_dir}/ss"

  if PATH="${stub_dir}:${PATH}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${env_file}" \
    SS_ARGS_LOG="${ss_log}" \
    SS_HTTP_PORT="80" \
    bash "${ONE_CLICK_DIR}/scripts/systemd/cube-proxy-postcheck.sh" >/dev/null 2>&1; then
    fail "expected postcheck to fail when gRPC port is not ready"
  fi

  assert_contains "${ss_log}" "sport = :80"
  assert_contains "${ss_log}" "sport = :9090"
}

test_postcheck_skips_when_external_host_set() {
  # When an external endpoint is configured the local container is never
  # started, so the postcheck must short-circuit with exit 0 instead of
  # blocking on wait_for_container_health (which would otherwise need docker
  # and ~80s before failing into Restart=on-failure).
  CUBE_EXTERNAL_MYSQL_HOST=db.example.com \
    bash "${ONE_CLICK_DIR}/scripts/systemd/mysql-postcheck.sh" \
    || fail "mysql-postcheck must exit 0 when CUBE_EXTERNAL_MYSQL_HOST is set"
  CUBE_EXTERNAL_REDIS_HOST=cache.example.com \
    bash "${ONE_CLICK_DIR}/scripts/systemd/redis-postcheck.sh" \
    || fail "redis-postcheck must exit 0 when CUBE_EXTERNAL_REDIS_HOST is set"
  # Sentinel-only external Redis has no fixed HOST; MASTER_NAME alone must
  # also skip the local container health wait.
  CUBE_EXTERNAL_REDIS_MASTER_NAME=mymaster \
    bash "${ONE_CLICK_DIR}/scripts/systemd/redis-postcheck.sh" \
    || fail "redis-postcheck must exit 0 when CUBE_EXTERNAL_REDIS_MASTER_NAME is set"
  CUBE_SANDBOX_MINIO_ENABLED=0 \
    bash "${ONE_CLICK_DIR}/scripts/systemd/minio-postcheck.sh" \
    || fail "minio-postcheck must exit 0 when CUBE_SANDBOX_MINIO_ENABLED=0"
}

test_external_redis_sentinel_wiring() {
  # Contract checks for "connect to an existing Sentinel" (no local Redis).
  # These are source-level assertions so they stay docker/redis-free like the
  # rest of this suite; they catch accidental drops of the Sentinel branch.
  local install_sh="${ONE_CLICK_DIR}/install.sh"
  local up_deps="${ONE_CLICK_DIR}/scripts/one-click/up-with-deps.sh"
  local up_support="${ONE_CLICK_DIR}/scripts/one-click/up-support.sh"
  local up_proxy="${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh"
  local up_lcm="${ONE_CLICK_DIR}/scripts/one-click/up-cube-lifecycle-manager.sh"
  local quickcheck="${ONE_CLICK_DIR}/scripts/one-click/quickcheck.sh"
  local env_example="${ONE_CLICK_DIR}/env.example"
  local global_conf="${ONE_CLICK_DIR}/cubeproxy/global.conf.template"
  local proxy_compose="${ONE_CLICK_DIR}/cubeproxy/docker-compose.yaml.template"
  local lcm_compose="${ONE_CLICK_DIR}/cube-lifecycle-manager/docker-compose.yaml.template"
  local postcheck="${ONE_CLICK_DIR}/scripts/systemd/redis-postcheck.sh"

  assert_contains "${env_example}" "CUBE_EXTERNAL_REDIS_MASTER_NAME"
  assert_contains "${env_example}" "CUBE_EXTERNAL_REDIS_SENTINEL_NODES"
  assert_contains "${env_example}" "CUBE_S3_ENDPOINT"
  assert_contains "${env_example}" "CUBE_SANDBOX_MINIO_ENABLED"
  assert_contains "${env_example}" "CUBE_SANDBOX_MINIO_IMAGE"
  assert_contains "${env_example}" "minio:RELEASE.2025-09-07T16-13-09Z"
  assert_contains "${up_support}" "minio:RELEASE.2025-09-07T16-13-09Z"
  assert_contains "${install_sh}" "fill_s3_from_local_minio"
  assert_contains "${install_sh}" "local_minio_s3_endpoint"
  assert_contains "${install_sh}" "check_minio_not_combined_with_user_s3"
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" "local_minio_s3_endpoint"
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" "s3_config_is_local_minio_fill"
  assert_contains "${install_sh}" "compute role does not deploy MinIO"
  if grep -A80 '^write_volume_s3_conf()' "${ONE_CLICK_DIR}/lib/common.sh" | grep -q 'CUBE_SANDBOX_MINIO_'; then
    fail "write_volume_s3_conf must read only CUBE_S3_* (not CUBE_SANDBOX_MINIO_*)"
  fi
  # The S3 volume plugin is a Go binary (examples/volume/s3). These are static
  # guards on the invariants that are easy to break by refactoring; the
  # behavioural assertions live in the module's own Go unit tests.
  local volume_s3_dir="${ONE_CLICK_DIR}/../../examples/volume/s3"
  local volume_s3_main="${volume_s3_dir}/cmd/cube-volume-s3/main.go"
  local volume_s3_api="${volume_s3_dir}/internal/s3api/client.go"
  local volume_s3_mount="${volume_s3_dir}/internal/s3fsmnt/mount.go"
  local volume_s3_lock="${volume_s3_dir}/internal/lockfile/lockfile.go"
  local volume_s3_config="${volume_s3_dir}/internal/config/config.go"
  assert_file "${volume_s3_main}"
  assert_file "${volume_s3_api}"
  assert_file "${volume_s3_mount}"
  assert_file "${volume_s3_lock}"
  assert_file "${volume_s3_config}"
  assert_file "${volume_s3_dir}/go.mod"

  # The control plane must not shell out to an S3 command line tool: dropping
  # the AWS CLI dependency is the whole point of the Go rewrite.
  if grep -rE 'exec\.Command\("aws"' "${volume_s3_dir}" --include='*.go' >/dev/null; then
    fail "cube-volume-s3 must not exec the aws CLI; use the minio-go client"
  fi

  # One process per hook, so the attach/detach lock has to be a file lock. A
  # sync.Mutex would silently stop serialising anything and let two sandboxes
  # double-mount the same volume.
  if ! grep -Fq 'syscall.Flock' "${volume_s3_lock}"; then
    fail "lockfile must use syscall.Flock (binary plugins get one process per op)"
  fi
  # LOCK_EX only appears on the acquire path, so this catches an acquire that
  # stops locking while Release still mentions Flock.
  if ! grep -Fq 'syscall.LOCK_EX' "${volume_s3_lock}"; then
    fail "lockfile must take an exclusive flock (syscall.LOCK_EX)"
  fi
  # Guard on the import rather than on "sync.Mutex", so the comment in
  # lockfile.go explaining why a mutex is wrong does not trip this check.
  if grep -rn '"sync"' "${volume_s3_dir}" --include='*.go' \
      | grep -v '_test\.go' >/dev/null; then
    fail "cube-volume-s3 must not import sync; attach/detach needs a cross-process flock"
  fi
  if ! grep -Fq 'lockfile.Acquire' "${volume_s3_main}"; then
    fail "attach/detach must acquire the per-volume lock"
  fi

  # s3fs 1.91+ stats the trailing-slash directory object when mounting a subdir,
  # so create must PUT "volumes/<id>/" (not a .keep file).
  if ! grep -Fq 'return "volumes/" + volumeID + "/"' "${volume_s3_config}"; then
    fail "VolumePrefix must be the trailing-slash directory object key"
  fi
  if ! grep -Fq 'config.VolumePrefix(volumeID)' "${volume_s3_api}"; then
    fail "CreateVolumeDir must PUT the trailing-slash directory object"
  fi
  if grep -Fq -- '-ocompat_dir' "${volume_s3_mount}"; then
    fail "s3fs mount must not hardcode -ocompat_dir (unknown on s3fs 1.90; create writes dir/ instead)"
  fi

  # Concurrent first creates race on MakeBucket; the loser must not fail.
  if ! grep -Fq 'BucketAlreadyOwnedByYou' "${volume_s3_api}"; then
    fail "EnsureBucket must treat BucketAlreadyOwnedByYou as success"
  fi
  if ! grep -Fq 'BucketAlreadyExists' "${volume_s3_api}"; then
    fail "EnsureBucket must treat BucketAlreadyExists as success"
  fi
  # AWS rejects LocationConstraint for us-east-1 and R2 (region=auto) rejects it
  # outright; minio-go omits it exactly when the location is us-east-1.
  if ! grep -A12 '^func BucketCreateRegion' "${volume_s3_api}" | grep -Fq '"us-east-1", "auto"'; then
    fail "BucketCreateRegion must map both us-east-1 and auto onto us-east-1"
  fi
  # destroy may only swallow not-found; anything else has to propagate so
  # CubeMaster does not drop the volume record while objects remain.
  if ! grep -A8 '^func IsNotFound' "${volume_s3_api}" | grep -Fq 'NoSuchBucket'; then
    fail "IsNotFound must treat NoSuchBucket as already gone"
  fi
  if ! grep -A8 '^func IsNotFound' "${volume_s3_api}" | grep -Fq 'NoSuchKey'; then
    fail "IsNotFound must treat NoSuchKey as already gone"
  fi
  assert_contains "${postcheck}" "CUBE_EXTERNAL_REDIS_MASTER_NAME"
  assert_contains "${up_support}" "CUBE_EXTERNAL_REDIS_MASTER_NAME"
  assert_contains "${up_support}" "CUBE_SANDBOX_MINIO_ENABLED"
  assert_contains "${up_support}" "minio"
  assert_contains "${up_support}" "SUPPORT_SERVICES:-mysql redis minio"
  assert_contains "${up_support}" "do not run compose down"
  assert_not_contains "${up_support}" "down --remove-orphans"
  assert_contains "${up_deps}" "CUBE_PROXY_REDIS_MASTER_NAME"
  assert_contains "${up_deps}" "CUBE_LCM_REDIS_MASTER_NAME"
  assert_contains "${up_deps}" "CUBE_PROXY_REDIS_SENTINEL_NODES"
  assert_contains "${up_lcm}" "CUBE_LCM_REDIS_MASTER_NAME"
  assert_contains "${up_proxy}" "CUBE_PROXY_REDIS_MASTER_NAME"
  assert_contains "${up_proxy}" "__CUBE_PROXY_REDIS_MASTER_NAME__"
  assert_contains "${up_proxy}" "__CUBE_PROXY_REGISTRY_REDIS_SENTINEL_NODES__"
  assert_contains "${quickcheck}" "EXTERNAL_REDIS_MASTER_NAME"
  assert_contains "${install_sh}" "CUBE_EXTERNAL_REDIS_MASTER_NAME"
  assert_contains "${install_sh}" "CUBE_EXTERNAL_REDIS_SENTINEL_NODES"
  assert_contains "${install_sh}" "SENTINEL get-master-addr-by-name"
  assert_contains "${install_sh}" "master_name:"
  assert_contains "${install_sh}" "sentinel_nodes:"
  # Leaving Sentinel mode must scrub conf.yaml keys (env side uses remove_env_kv).
  assert_contains "${install_sh}" "removing stale Redis Sentinel keys from conf.yaml"
  assert_contains "${install_sh}" "/^  master_name:/d; /^  sentinel_nodes:/d; /^  sentinel_password:/d"
  # Back to bundled Redis must also drop external Redis markers from .one-click.env
  # and persist CUBE_SANDBOX_REDIS_PASSWORD (extracted into common.sh).
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" "Back to bundled local Redis: drop every external Redis marker"
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" 'CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123'
  assert_contains "${install_sh}" 'persist_one_click_redis_runtime_env "${RUNTIME_ENV_FILE}"'
  # SENTINEL lookup must reuse credentials without putting the password in argv.
  assert_contains "${install_sh}" 'REDISCLI_AUTH="${sentinel_pd}"'
  # Leaving Sentinel for bundled Redis must restore password as well as nodes.
  assert_contains "${install_sh}" "restoring bundled Redis nodes/password in conf.yaml"
  # Sentinel → external standalone must scrub leftover master_name/sentinel_* keys.
  assert_contains "${install_sh}" "Sentinel → standalone: drop leftover master_name/sentinel_*"
  # Empty CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD must not AUTH with the master password.
  if grep -E 'sentinel_pd="\$\{CUBE_EXTERNAL_REDIS_PASSWORD\}"' "${install_sh}" >/dev/null; then
    fail "install.sh must not fall back Sentinel AUTH to CUBE_EXTERNAL_REDIS_PASSWORD"
  fi
  assert_contains "${env_example}" "Leave SENTINEL_PASSWORD empty"
  assert_contains "${global_conf}" "redis_master_name"
  assert_contains "${global_conf}" "redis_sentinel_nodes"
  assert_contains "${proxy_compose}" "CUBE_PROXY_REGISTRY_REDIS_MASTER_NAME"
  assert_contains "${proxy_compose}" "CUBE_PROXY_REGISTRY_REDIS_SENTINEL_NODES"
  assert_contains "${lcm_compose}" "CUBE_LCM_REDIS_MASTER_NAME"
  assert_contains "${lcm_compose}" "CUBE_LCM_REDIS_SENTINEL_NODES"
}

test_webui_postcheck_skips_when_disabled() {
  local stub_dir="${TMP_DIR}/webui-disabled/bin"

  mkdir -p "${stub_dir}"
  cat > "${stub_dir}/ss" <<'SH'
#!/usr/bin/env bash
echo "ss must not be called when WebUI is disabled" >&2
exit 1
SH
  chmod +x "${stub_dir}/ss"

  PATH="${stub_dir}:${PATH}" \
    WEB_UI_ENABLE=0 \
    bash "${ONE_CLICK_DIR}/scripts/systemd/webui-postcheck.sh" \
    || fail "webui-postcheck must exit 0 when WEB_UI_ENABLE is disabled"
}

test_mask_external_dep_services_remove_then_mask() {
  local path="${ONE_CLICK_DIR}/install.sh"

  # Both local dependency units are routed through the shared masking helper.
  assert_contains "${path}" "mask_local_dep_service cube-sandbox-mysql.service"
  assert_contains "${path}" "mask_local_dep_service cube-sandbox-redis.service"
  assert_contains "${path}" "mask_local_dep_service cube-sandbox-minio.service"
  # Core fix: remove the installed regular file BEFORE masking, otherwise plain
  # `systemctl mask` fails to overlay its /dev/null symlink on an existing file.
  assert_contains "${path}" "rm -f \"\${unit_dir}/\${unit}\""
  assert_contains "${path}" "systemctl mask \"\${unit}\""
  # Switch-back-to-local path still unmasks both units, and a daemon-reload
  # follows so the new (un)masked state is picked up before the target starts.
  assert_contains "${path}" "systemctl unmask cube-sandbox-mysql.service"
  assert_contains "${path}" "systemctl unmask cube-sandbox-redis.service"
  assert_contains "${path}" "systemctl unmask cube-sandbox-minio.service"
  assert_contains "${path}" "systemctl daemon-reload"
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
test_quickcheck_probes_are_race_tolerant
test_quickcheck_wait_until_retries_then_succeeds
test_quickcheck_wait_until_times_out_with_descriptive_die
test_quickcheck_timeout_zero_is_fail_fast
test_quickcheck_invalid_budget_falls_back_to_defaults
test_quickcheck_budget_normalizes_leading_zeros
test_quickcheck_interval_clamped_to_timeout
test_quickcheck_timeout_clamped_to_max
test_quickcheck_timeout_overflow_falls_back_to_default
test_quickcheck_check_executable_behaviour
test_quickcheck_bind_mount_file_uses_specific_message
test_quickcheck_node_registration_prefers_missing_ip_reason
test_quickcheck_node_registration_reports_unreachable_reason
test_quickcheck_node_registration_keeps_missing_ip_after_blip
test_quickcheck_container_ready_retries_transient_states
test_quickcheck_container_ready_dies_on_terminal_status
test_quickcheck_container_ready_bounded_by_overall_deadline
test_quickcheck_container_ready_dies_on_per_container_timeout
test_quickcheck_container_ready_accepts_healthy_status
test_quickcheck_budget_is_shared_across_sequential_probes
test_quickcheck_callers_do_not_wrap_in_retry_loop
test_quickcheck_node_registration_succeeds_on_first_match
test_quickcheck_node_registration_response_missing_ip_field
test_quickcheck_check_socket_retries_then_succeeds
test_quickcheck_check_http_retries_then_succeeds
test_quickcheck_check_unit_active_dies_fast_on_failed_unit
test_cube_proxy_postcheck_defaults_to_http_port_80
test_cube_proxy_postcheck_follows_http_port
test_cube_proxy_postcheck_ignores_https_port
test_cube_proxy_postcheck_ignores_deprecated_host_port
test_cube_proxy_postcheck_prefers_http_over_deprecated_host_port
test_cube_proxy_postcheck_follows_grpc_port
test_cube_proxy_postcheck_fails_when_grpc_port_not_ready
test_postcheck_skips_when_external_host_set
test_webui_postcheck_skips_when_disabled
test_external_redis_sentinel_wiring
test_mask_external_dep_services_remove_then_mask

echo "runtime file safety tests OK"
