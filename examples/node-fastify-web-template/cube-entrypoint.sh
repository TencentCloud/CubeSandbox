#!/bin/sh
# Start CubeSandbox envd and an optional user command.

set -eu

ENVD_BIN="${ENVD_BIN:-/usr/bin/envd}"
ENVD_PORT="${ENVD_PORT:-49983}"
ENVD_LOG_FILE="${ENVD_LOG_FILE:-/var/log/envd.log}"
ENVD_EXTRA_ARGS="${ENVD_EXTRA_ARGS:-}"

if [ ! -x "${ENVD_BIN}" ]; then
    echo "cube-entrypoint: envd binary not found or not executable at ${ENVD_BIN}" >&2
    exit 127
fi

start_envd() {
    # shellcheck disable=SC2086
    if [ "${ENVD_LOG_FILE}" = "-" ]; then
        "${ENVD_BIN}" -port "${ENVD_PORT}" ${ENVD_EXTRA_ARGS} &
    else
        mkdir -p "$(dirname "${ENVD_LOG_FILE}")"
        "${ENVD_BIN}" -port "${ENVD_PORT}" ${ENVD_EXTRA_ARGS} \
            >>"${ENVD_LOG_FILE}" 2>&1 &
    fi
    ENVD_PID=$!
    echo "cube-entrypoint: started envd (pid=${ENVD_PID}) on port ${ENVD_PORT}" >&2
}

wait_for_process() {
    pid="$1"
    rc=0

    # dash interrupts wait after running a signal trap, even when the child is
    # still alive. Keep waiting so graceful shutdown can actually finish.
    while :; do
        if wait "${pid}"; then
            rc=0
        else
            rc=$?
        fi
        if [ "${rc}" -lt 128 ] || ! kill -0 "${pid}" 2>/dev/null; then
            break
        fi
    done

    return "${rc}"
}

start_envd

if [ "$#" -eq 0 ]; then
    if wait_for_process "${ENVD_PID}"; then
        rc=0
    else
        rc=$?
    fi
    exit "${rc}"
fi

USER_PID=""
forward_signal() {
    sig="$1"
    if [ -n "${USER_PID}" ]; then
        kill -s "${sig}" "${USER_PID}" 2>/dev/null || true
    fi
}

trap 'forward_signal TERM' TERM
trap 'forward_signal INT' INT
trap 'forward_signal HUP' HUP

"$@" &
USER_PID=$!
echo "cube-entrypoint: exec user command (pid=${USER_PID}): $*" >&2

if wait_for_process "${USER_PID}"; then
    rc=0
else
    rc=$?
fi
exit "${rc}"
