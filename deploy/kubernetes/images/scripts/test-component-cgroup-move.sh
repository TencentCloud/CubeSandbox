#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck source=component-entrypoint.sh
CUBE_COMPONENT_ENTRYPOINT_LIB_ONLY=true source "${SCRIPT_DIR}/component-entrypoint.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "${test_root}"' EXIT

mkdir -p \
  "${test_root}/v2/cube-sandbox-runtime" \
  "${test_root}/proc-v2/123" \
  "${test_root}/v1/cpu/cube-sandbox-runtime" \
  "${test_root}/proc-v1/456"
touch \
  "${test_root}/v2/cgroup.controllers" \
  "${test_root}/v2/cgroup.procs" \
  "${test_root}/v2/cube-sandbox-runtime/cgroup.procs" \
  "${test_root}/v1/cpu/tasks" \
  "${test_root}/v1/cpu/cube-sandbox-runtime/tasks"
printf '0::/cube-sandbox-runtime\n' > "${test_root}/proc-v2/123/cgroup"
printf '2:cpu:/cube-sandbox-runtime\n' > "${test_root}/proc-v1/456/cgroup"

CUBE_CGROUP_ROOT="${test_root}/v2" \
CUBE_PROC_ROOT="${test_root}/proc-v2" \
  move_pid_to_host_cgroups 123
[[ "$(cat "${test_root}/v2/cube-sandbox-runtime/cgroup.procs")" == "123" ]]

CUBE_CGROUP_ROOT="${test_root}/v1" \
CUBE_PROC_ROOT="${test_root}/proc-v1" \
  move_pid_to_host_cgroups 456
[[ "$(cat "${test_root}/v1/cpu/cube-sandbox-runtime/tasks")" == "456" ]]

CUBE_PID_DIR="${test_root}/pid"
mkdir -p "${CUBE_PID_DIR}"

sleep 300 &
owned_pid=$!
owned_bin="$(readlink "/proc/${owned_pid}/exe")"
write_pidfile test-component "${owned_pid}"
read -r recorded_pid recorded_starttime < "${CUBE_PID_DIR}/test-component.pid"
[[ "${recorded_pid}" == "${owned_pid}" ]]
[[ "${recorded_starttime}" =~ ^[0-9]+$ ]]
process_identity_matches "${owned_pid}" "${recorded_starttime}" \
  "/a/different/mount/namespace/$(basename "${owned_bin}")"

# A reused PID has a different starttime and must never receive TERM.
printf '%s %s\n' "${owned_pid}" "$((recorded_starttime + 1))" \
  > "${CUBE_PID_DIR}/test-component.pid"
stop_owned_process test-component "${owned_bin}" 1
kill -0 "${owned_pid}"
[[ ! -f "${CUBE_PID_DIR}/test-component.pid" ]]

# A matching PID/starttime with a different executable name must not be killed.
write_pidfile test-component "${owned_pid}"
stop_owned_process test-component /not-the-owned-process 1
kill -0 "${owned_pid}"

write_pidfile test-component "${owned_pid}"
stop_owned_process test-component "${owned_bin}" 2
if kill -0 "${owned_pid}" 2>/dev/null; then
  printf 'owned process was not stopped\n' >&2
  exit 1
fi
wait "${owned_pid}" 2>/dev/null || true

# A launcher that fails while stopped must be continued and reaped.
launch_pid_file="${test_root}/launch.pid"
(
  (
    kill -STOP "${BASHPID}"
    exec sleep 300
  ) &
  STARTING_PID=$!
  printf '%s\n' "${STARTING_PID}" > "${launch_pid_file}"
  STARTING_STARTTIME="$(awk '{print $22}' "/proc/${STARTING_PID}/stat")"
  cleanup_starting_process
)
failed_launch_pid="$(cat "${launch_pid_file}")"
if kill -0 "${failed_launch_pid}" 2>/dev/null; then
  printf 'failed stopped launcher was not cleaned up\n' >&2
  exit 1
fi

# A daemonized child remains in the private launch group after its leader exits.
# Cleanup must remove the group, not only the original launcher PID.
group_child_file="${test_root}/group-child.pid"
(
  setsid bash -c '
    sleep 300 &
    printf "%s\n" "$!" > "$1"
  ' cube-component-launcher "${group_child_file}" &
  STARTING_PID=$!
  STARTING_STARTTIME="$(awk '{print $22}' "/proc/${STARTING_PID}/stat")"
  STARTING_PGID="${STARTING_PID}"
  wait "${STARTING_PID}" 2>/dev/null || true
  cleanup_starting_process
)
group_child_pid="$(cat "${group_child_file}")"
if kill -0 "${group_child_pid}" 2>/dev/null; then
  printf 'daemonized launch-group child was not cleaned up\n' >&2
  exit 1
fi

printf 'component cgroup move and process identity tests passed\n'
