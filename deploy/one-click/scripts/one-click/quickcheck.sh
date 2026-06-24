#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

# Readiness budget for the post-start quickcheck. quickcheck runs immediately
# after the units are (re)started -- e.g. install.sh's `systemctl enable --now
# <target>` -- but systemd considers a service "started" as soon as its
# ExecStart is launched: the cubelet / network-agent daemons still need a brief
# moment to bind their unix sockets (/data/cubelet/cubelet.sock,
# /tmp/cube/network-agent-grpc.sock) and serve their HTTP health endpoints.
# Probing exactly once therefore loses a startup race and returns a
# false-negative install failure even though the node comes up healthy seconds
# later.
#
# To remove that whole class of flakes for every caller (install.sh, smoke.sh,
# deploy-manual.sh, up.sh, up-compute.sh), the probes are retried until they
# pass or a single, shared wall-clock budget (QUICKCHECK_DEADLINE) is exhausted.
# The budget is shared across all probes in one run -- not reset per probe -- so
# the whole script is bounded by roughly CUBE_QUICKCHECK_READY_TIMEOUT seconds
# regardless of how many probes run, and callers must not wrap quickcheck in
# their own retry loop (that would multiply the budget on a genuinely broken
# node). Set CUBE_QUICKCHECK_READY_TIMEOUT=0 to restore strict fail-fast (probe
# exactly once) behaviour.
#
# The default is intentionally generous: the cold-start callers (install.sh,
# smoke.sh, deploy-manual.sh) invoke quickcheck right after (re)starting the
# units with no prior readiness wait, so the budget must cover the cumulative
# time for units to activate, sockets to bind, health endpoints to serve, and
# (on compute) the node to register with a remote cubemaster.
QUICKCHECK_READY_TIMEOUT_DEFAULT=120
QUICKCHECK_READY_INTERVAL_DEFAULT=2
# Upper clamp so a typo/overflow cannot turn the budget into an effectively
# infinite wait.
QUICKCHECK_READY_TIMEOUT_MAX=3600
# Per-request curl bounds so a black-holed endpoint (host up, port dropping
# SYNs) cannot block a probe far past the readiness budget.
QUICKCHECK_CURL_CONNECT_TIMEOUT=5
QUICKCHECK_CURL_MAX_TIME=10
# Per-call bound for `docker inspect` so a wedged docker daemon cannot block a
# container probe indefinitely. Unlike curl, `docker inspect` has no built-in
# request timeout, and the shared deadline is only re-checked between iterations,
# so an unbounded inspect call could hang the whole run past QUICKCHECK_DEADLINE.
QUICKCHECK_DOCKER_TIMEOUT=10
# Cap the buffered node-registration response (it is a small JSON document).
QUICKCHECK_CURL_MAX_FILESIZE=1048576

# Normalise CUBE_QUICKCHECK_READY_TIMEOUT / _INTERVAL into validated integers and
# compute the single shared deadline. Numeric values are forced to base 10 so a
# leading-zero value (e.g. "08") is not mis-parsed as octal and aborted under
# `set -e`. Invalid values fall back to the defaults with a warning so an
# operator typo is visible instead of silently ignored.
quickcheck_init_budget() {
  local raw_timeout="${CUBE_QUICKCHECK_READY_TIMEOUT:-${QUICKCHECK_READY_TIMEOUT_DEFAULT}}"
  local raw_interval="${CUBE_QUICKCHECK_READY_INTERVAL:-${QUICKCHECK_READY_INTERVAL_DEFAULT}}"
  local timeout interval

  QUICKCHECK_READY_TIMEOUT="$(quickcheck_sanitize_seconds \
    "${raw_timeout}" "CUBE_QUICKCHECK_READY_TIMEOUT" "${QUICKCHECK_READY_TIMEOUT_DEFAULT}" 0)"
  QUICKCHECK_READY_INTERVAL="$(quickcheck_sanitize_seconds \
    "${raw_interval}" "CUBE_QUICKCHECK_READY_INTERVAL" "${QUICKCHECK_READY_INTERVAL_DEFAULT}" 1)"
  timeout="${QUICKCHECK_READY_TIMEOUT}"
  interval="${QUICKCHECK_READY_INTERVAL}"

  # Never sleep past the overall budget.
  if (( timeout > 0 && interval > timeout )); then
    QUICKCHECK_READY_INTERVAL="${timeout}"
  fi

  QUICKCHECK_DEADLINE=$(( $(date +%s) + QUICKCHECK_READY_TIMEOUT ))
}

# Normalise a seconds value from operator-controlled env into a validated base-10
# integer in [min, QUICKCHECK_READY_TIMEOUT_MAX], falling back to a default with a
# warning. The `^[0-9]{1,7}$` pattern rejects empty / non-numeric / overflowing
# (>7 digit) input in one shot, which both prevents an octal abort under `set -e`
# (leading zeros via $((10#...))) and keeps the conversion well within int64 so it
# cannot wrap into a negative/garbage budget. Echoes the sanitized integer.
quickcheck_sanitize_seconds() {
  local raw="$1"
  local name="$2"
  local default="$3"
  local min="$4"
  local value

  if [[ "${raw}" =~ ^[0-9]{1,7}$ ]]; then
    value=$((10#${raw}))
  else
    log "ignoring invalid ${name}='${raw}'; using default ${default}s"
    value="${default}"
  fi
  if (( value < min )); then
    log "ignoring out-of-range ${name}='${raw}'; using default ${default}s"
    value="${default}"
  fi
  if (( value > QUICKCHECK_READY_TIMEOUT_MAX )); then
    log "clamping ${name}='${raw}' to ${QUICKCHECK_READY_TIMEOUT_MAX}s"
    value="${QUICKCHECK_READY_TIMEOUT_MAX}"
  fi
  printf '%s\n' "${value}"
}

# Retry an arbitrary predicate command until it succeeds or the shared readiness
# budget is exhausted, then die with a descriptive message. The predicate must
# return a status (it must NOT call die itself), so transient failures can be
# retried.
wait_until() {
  local desc="$1"
  shift
  while :; do
    if "$@"; then
      return 0
    fi
    if (( $(date +%s) >= QUICKCHECK_DEADLINE )); then
      die "${desc} (not ready within ${QUICKCHECK_READY_TIMEOUT}s)"
    fi
    # A sleep interrupted by a signal exits non-zero; tolerate it so `set -e`
    # does not abort with a generic "sleep: interrupted" instead of the
    # descriptive readiness failure the next deadline check would report.
    sleep "${QUICKCHECK_READY_INTERVAL}" || true
  done
}

unit_is_active() {
  systemctl is-active --quiet "$1"
}

check_unit_active() {
  local unit="$1"
  wait_until "expected systemd unit not active: ${unit}" unit_is_active "${unit}"
}

http_ok() {
  curl -fsS \
    --connect-timeout "${QUICKCHECK_CURL_CONNECT_TIMEOUT}" \
    --max-time "${QUICKCHECK_CURL_MAX_TIME}" \
    "$1" >/dev/null 2>&1
}

check_http() {
  local url="$1"
  wait_until "endpoint not healthy: ${url}" http_ok "${url}"
}

check_socket() {
  local path="$1"
  wait_until "expected socket not ready: ${path}" test -S "${path}"
}

check_file() {
  local path="$1"
  local desc="${2:-expected file not ready: ${path}}"
  wait_until "${desc}" test -f "${path}"
}

check_executable() {
  local path="$1"
  wait_until "expected executable not ready: ${path}" test -x "${path}"
}

# Read a container's effective status: its healthcheck status when a healthcheck
# is defined, otherwise its lifecycle status. Bounded by QUICKCHECK_DOCKER_TIMEOUT
# via `timeout` when available so a wedged docker daemon cannot hang the probe
# past the shared deadline (which is only re-checked between iterations). Echoes
# the status string (empty on any failure); never dies, so callers can retry.
quickcheck_container_status() {
  local container="$1"
  local fmt='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}'
  # `timeout` can only bound an external binary, not a shell function, and
  # functions are not inherited by this standalone script in production -- so a
  # `docker` function only exists when the tests stub it. Skip the wrapper in
  # that case so the stub is still invoked; in production docker is the external
  # binary and gets the bound. `-k 5` forces SIGKILL if a wedged inspect ignores
  # the initial SIGTERM. `--` stops a `-`-leading container-name override from
  # being parsed as a docker option.
  if ! declare -F docker >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
    timeout -k 5 "${QUICKCHECK_DOCKER_TIMEOUT}" docker inspect --format "${fmt}" -- "${container}" 2>/dev/null || true
  else
    docker inspect --format "${fmt}" -- "${container}" 2>/dev/null || true
  fi
}

check_container_ready() {
  local container="$1"
  # The per-container budget defaults to the overall readiness budget so a
  # slow-but-healthy container (e.g. first-boot MySQL initialising its data dir)
  # is not failed early just because the per-container default was lower than the
  # generous overall budget. Set CUBE_QUICKCHECK_CONTAINER_TIMEOUT lower to fail a
  # wedged container sooner; it is capped by the shared QUICKCHECK_DEADLINE below
  # either way.
  local container_timeout
  container_timeout="$(quickcheck_sanitize_seconds \
    "${CUBE_QUICKCHECK_CONTAINER_TIMEOUT:-${QUICKCHECK_READY_TIMEOUT}}" \
    "CUBE_QUICKCHECK_CONTAINER_TIMEOUT" "${QUICKCHECK_READY_TIMEOUT}" 0)"
  local start now status
  # Measure the per-container budget as wall-clock elapsed (not a sleep-interval
  # accumulator): each probe can spend up to QUICKCHECK_DOCKER_TIMEOUT inside
  # `docker inspect`, so counting only the interval would let a slow daemon blow
  # well past CUBE_QUICKCHECK_CONTAINER_TIMEOUT before it trips.
  start="$(date +%s)"
  while :; do
    status="$(quickcheck_container_status "${container}")"
    case "${status}" in
      healthy|running)
        return 0
        ;;
      # Transient startup states (including an empty status, when the container
      # has been launched but not yet created): keep waiting until a budget is
      # exhausted, like every other probe.
      starting|created|restarting|"")
        ;;
      *)
        die "container is not ready: ${container} (status=${status:-unknown})"
        ;;
    esac
    # Bounded by the per-container budget AND the shared overall deadline, so a
    # wedged container fails fast without letting container checks blow past the
    # whole-run budget. Attribute the failure to whichever bound tripped.
    now="$(date +%s)"
    if (( now - start >= container_timeout )); then
      die "container is not ready within ${container_timeout}s: ${container} (status=${status:-unknown})"
    fi
    if (( now >= QUICKCHECK_DEADLINE )); then
      die "container is not ready within the ${QUICKCHECK_READY_TIMEOUT}s overall quickcheck budget: ${container} (status=${status:-unknown})"
    fi
    # Tolerate a signal-interrupted sleep so `set -e` does not abort here; the
    # next iteration's deadline check reports the descriptive failure instead.
    sleep "${QUICKCHECK_READY_INTERVAL}" || true
  done
}

check_bind_mount_source_file() {
  local path="$1"
  check_file "${path}" "expected bind mount source file not ready: ${path}"
}

# Wait for the node to register with cubemaster. A dedicated loop (rather than
# the generic wait_until) so the final failure preserves the distinction between
# "could not reach cubemaster" and "registered but missing host_ip", which is
# the difference between a connectivity problem and a cubelet/identity problem.
# Once cubemaster has been reached at least once the more diagnostic
# "missing host_ip" reason is kept sticky, so a momentary connectivity blip on
# the final attempt does not mask the real (registration) problem.
check_node_registration() {
  local node_id="$1"
  local master_addr="$2"
  local registration
  local reached=0
  local last_reason="failed to query cubemaster node registration for ${node_id}"
  while :; do
    if registration="$(curl -fsS \
        --connect-timeout "${QUICKCHECK_CURL_CONNECT_TIMEOUT}" \
        --max-time "${QUICKCHECK_CURL_MAX_TIME}" \
        --max-filesize "${QUICKCHECK_CURL_MAX_FILESIZE}" \
        "http://${master_addr}/internal/meta/nodes/${node_id}" 2>/dev/null)"; then
      if grep -Fq "\"host_ip\":\"${node_id}\"" <<<"${registration}"; then
        return 0
      fi
      if grep -Fq '"host_ip":"' <<<"${registration}"; then
        reached=1
        last_reason="cubemaster node registration missing host_ip=${node_id}"
      elif (( reached == 0 )); then
        last_reason="cubemaster node registration response missing host_ip field for ${node_id}"
      fi
    elif (( reached == 0 )); then
      last_reason="failed to query cubemaster node registration for ${node_id}"
    fi
    if (( $(date +%s) >= QUICKCHECK_DEADLINE )); then
      die "${last_reason} (not ready within ${QUICKCHECK_READY_TIMEOUT}s)"
    fi
    # Tolerate a signal-interrupted sleep so `set -e` does not abort here; the
    # next iteration's deadline check reports the descriptive failure instead.
    sleep "${QUICKCHECK_READY_INTERVAL}" || true
  done
}

quickcheck_main() {
  require_cmd systemctl
  require_cmd curl
  require_cmd grep

  local TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
  local MASTER_ADDR
  MASTER_ADDR="$(resolve_control_plane_cubemaster_addr)"
  local NA_HEALTH_ADDR="${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}"
  local CUBE_API_HEALTH_ADDR="${CUBE_API_HEALTH_ADDR:-127.0.0.1:3000}"
  local ROLE
  ROLE="$(one_click_deploy_role)"
  local NODE_ID="${CUBE_SANDBOX_NODE_IP:-}"

  # When external MySQL/Redis is configured the local container + systemd unit do
  # not exist, so the corresponding checks must be skipped.
  local EXTERNAL_MYSQL_HOST="${CUBE_EXTERNAL_MYSQL_HOST:-}"
  local EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"

  # Validate the host:port / IP values before they are interpolated into curl
  # URLs and grep patterns. resolve_control_plane_cubemaster_addr already
  # validates the compute-role address, but the control-plane default and the
  # health-endpoint overrides reach curl unchecked otherwise.
  validate_host_port "${MASTER_ADDR}" "cubemaster address"
  validate_host_port "${NA_HEALTH_ADDR}" "NETWORK_AGENT_HEALTH_ADDR"
  if [[ "${ROLE}" != "compute" ]]; then
    validate_host_port "${CUBE_API_HEALTH_ADDR}" "CUBE_API_HEALTH_ADDR"
  fi

  quickcheck_init_budget

  echo "[quickcheck] role=${ROLE}"
  echo "[quickcheck] cubemaster=${MASTER_ADDR}"
  echo "[quickcheck] network-agent-health=${NA_HEALTH_ADDR}"
  if [[ "${ROLE}" != "compute" ]]; then
    echo "[quickcheck] cube-api-health=${CUBE_API_HEALTH_ADDR}"
  fi

  echo "[quickcheck] check systemd units"
  check_unit_active cube-sandbox-network-agent.service
  check_unit_active cube-sandbox-cubelet.service
  if [[ "${ROLE}" != "compute" ]]; then
    if [[ -n "${EXTERNAL_MYSQL_HOST}" ]]; then
      echo "[quickcheck] external MySQL (${EXTERNAL_MYSQL_HOST}); skipping local mysql unit check"
    else
      check_unit_active cube-sandbox-mysql.service
    fi
    if [[ -n "${EXTERNAL_REDIS_HOST}" ]]; then
      echo "[quickcheck] external Redis (${EXTERNAL_REDIS_HOST}); skipping local redis unit check"
    else
      check_unit_active cube-sandbox-redis.service
    fi
    check_unit_active cube-sandbox-cubemaster.service
    check_unit_active cube-sandbox-cube-api.service
    check_unit_active cube-sandbox-cube-proxy.service
    check_unit_active cube-sandbox-coredns.service
    check_unit_active cube-sandbox-dns.service
    if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
      check_unit_active cube-sandbox-webui.service
    fi
  fi

  if command -v docker >/dev/null 2>&1 && [[ "${ROLE}" != "compute" ]]; then
    echo "[quickcheck] check container runtime state"
    [[ -n "${EXTERNAL_MYSQL_HOST}" ]] || check_container_ready "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
    [[ -n "${EXTERNAL_REDIS_HOST}" ]] || check_container_ready "${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}"
    check_container_ready "${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"
    check_container_ready "${CUBE_PROXY_COREDNS_CONTAINER:-cube-proxy-coredns}"
    if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
      check_container_ready "${WEB_UI_CONTAINER_NAME:-cube-webui}"
    fi
  fi

  echo "[quickcheck] 1/5 check network-agent healthz"
  check_http "http://${NA_HEALTH_ADDR}/healthz"

  echo "[quickcheck] 2/5 check network-agent readyz"
  check_http "http://${NA_HEALTH_ADDR}/readyz"

  echo "[quickcheck] 3/5 check cubemaster /notify/health"
  check_http "http://${MASTER_ADDR}/notify/health"

  if [[ "${ROLE}" == "compute" ]]; then
    [[ -n "${NODE_ID}" ]] || die "CUBE_SANDBOX_NODE_IP is required for compute quickcheck"
    validate_ipv4_literal "${NODE_ID}" "CUBE_SANDBOX_NODE_IP"
    echo "[quickcheck] 4/5 check cubemaster node registration"
    check_node_registration "${NODE_ID}" "${MASTER_ADDR}"

    echo "[quickcheck] 5/5 check essential sockets and runtime assets"
    check_socket "/data/cubelet/cubelet.sock"
    check_socket "/tmp/cube/network-agent-grpc.sock"
    check_file "${TOOLBOX_ROOT}/Cubelet/config/config.toml"
    check_file "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
    check_file "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml"
    check_file "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
    check_file "${TOOLBOX_ROOT}/cube-image/cube-guest-image-cpu.img"
  else
    echo "[quickcheck] 4/5 check cube-api /health"
    check_http "http://${CUBE_API_HEALTH_ADDR}/health"

    echo "[quickcheck] 5/5 check essential sockets and config"
    check_socket "/data/cubelet/cubelet.sock"
    check_socket "/tmp/cube/network-agent-grpc.sock"
    check_executable "${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
    check_file "${TOOLBOX_ROOT}/CubeMaster/conf.yaml"
    check_file "${TOOLBOX_ROOT}/Cubelet/config/config.toml"
    check_file "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
    check_file "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml"
    check_bind_mount_source_file "${TOOLBOX_ROOT}/cubeproxy/global.conf"
    check_bind_mount_source_file "${TOOLBOX_ROOT}/cubeproxy/nginx.conf"
    check_bind_mount_source_file "${TOOLBOX_ROOT}/coredns/Corefile"
    check_bind_mount_source_file "${TOOLBOX_ROOT}/coredns/resolv.conf.upstream"
    if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
      check_bind_mount_source_file "${TOOLBOX_ROOT}/webui/nginx.generated.conf"
    fi
  fi

  echo "[quickcheck] OK"
}

# Allow tests to source this file for its helper functions without executing the
# probe sequence; only run when invoked directly.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  quickcheck_main "$@"
fi
