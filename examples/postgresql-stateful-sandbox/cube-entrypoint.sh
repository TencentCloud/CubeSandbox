#!/bin/sh
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

# Supervise envd beside the PostgreSQL process. Both children are reaped, envd
# startup failures stop PostgreSQL, and TERM/INT wait for PostgreSQL to complete
# its fast shutdown before the container exits.

set -eu

ENVD_BIN="${ENVD_BIN:-/usr/bin/envd}"
ENVD_PORT="${ENVD_PORT:-49983}"
ENVD_LOG_FILE="${ENVD_LOG_FILE:-/var/log/envd.log}"

ENVD_PID=""
USER_PID=""
PENDING_SIGNAL=""
WAIT_STATUS=0

if [ ! -x "${ENVD_BIN}" ]; then
    echo "cube-entrypoint: envd binary not found or not executable at ${ENVD_BIN}" >&2
    exit 127
fi

child_is_running() {
    [ -n "$1" ] && kill -0 "$1" 2>/dev/null
}

wait_for_child() {
    wait_child_pid="$1"
    while true; do
        set +e
        wait "${wait_child_pid}"
        WAIT_STATUS=$?
        set -e
        if child_is_running "${wait_child_pid}"; then
            continue
        fi
        return
    done
}

start_envd() {
    if [ "${ENVD_LOG_FILE}" = "-" ]; then
        "${ENVD_BIN}" -port "${ENVD_PORT}" &
    else
        mkdir -p "$(dirname "${ENVD_LOG_FILE}")"
        "${ENVD_BIN}" -port "${ENVD_PORT}" \
            >>"${ENVD_LOG_FILE}" 2>&1 &
    fi
    ENVD_PID=$!
    echo "cube-entrypoint: started envd (pid=${ENVD_PID}) on port ${ENVD_PORT}" >&2
}

stop_envd() {
    if [ -z "${ENVD_PID}" ]; then
        return
    fi
    if child_is_running "${ENVD_PID}"; then
        kill -TERM "${ENVD_PID}" 2>/dev/null || true
    fi
    wait_for_child "${ENVD_PID}"
    ENVD_PID=""
}

queue_signal() {
    queued_signal="$1"
    case "${PENDING_SIGNAL}:${queued_signal}" in
        TERM:*|INT:*)
            ;;
        *:TERM|*:INT)
            PENDING_SIGNAL="${queued_signal}"
            ;;
        *)
            PENDING_SIGNAL="${queued_signal}"
            ;;
    esac
}

# Install queueing traps before either child is launched. POSIX sh cannot block
# signals around "$@" and "$!" atomically, so a signal received during startup
# is handled after the child PID has been recorded.
trap 'queue_signal TERM' TERM
trap 'queue_signal INT' INT
trap 'queue_signal HUP' HUP

start_envd

if [ "$#" -eq 0 ]; then
    trap 'stop_envd; exit 0' TERM INT
    trap ':' HUP
    case "${PENDING_SIGNAL}" in
        TERM|INT)
            stop_envd
            exit 0
            ;;
    esac
    wait_for_child "${ENVD_PID}"
    rc=${WAIT_STATUS}
    ENVD_PID=""
    exit "${rc}"
fi

# Invoked by the TERM/INT traps below.
# shellcheck disable=SC2329
shutdown() {
    shutdown_signal="$1"
    trap '' TERM INT HUP

    if [ -n "${USER_PID}" ]; then
        if child_is_running "${USER_PID}"; then
            kill -s "${shutdown_signal}" "${USER_PID}" 2>/dev/null || true
        fi
        wait_for_child "${USER_PID}"
        rc=${WAIT_STATUS}
        USER_PID=""
    else
        rc=0
    fi

    stop_envd
    exit "${rc}"
}

# Invoked by the HUP trap below.
# shellcheck disable=SC2329
forward_hup() {
    if child_is_running "${USER_PID}"; then
        kill -HUP "${USER_PID}" 2>/dev/null || true
    fi
}

case "${PENDING_SIGNAL}" in
    TERM|INT)
        stop_envd
        exit 0
        ;;
esac

"$@" &
USER_PID=$!
echo "cube-entrypoint: exec user command (pid=${USER_PID}): $*" >&2

trap 'shutdown TERM' TERM
trap 'shutdown INT' INT
trap 'forward_hup' HUP

startup_signal=${PENDING_SIGNAL}
PENDING_SIGNAL=""
case "${startup_signal}" in
    TERM|INT)
        shutdown "${startup_signal}"
        ;;
    HUP)
        forward_hup
        ;;
esac

# dash has no portable wait -n. Poll both children, then reap whichever owns
# the lifecycle transition. A one-second interval is sufficient for startup
# failure propagation without introducing a shell-specific dependency.
while child_is_running "${USER_PID}" && child_is_running "${ENVD_PID}"; do
    sleep 1 || true
done

if ! child_is_running "${USER_PID}"; then
    wait_for_child "${USER_PID}"
    rc=${WAIT_STATUS}
    USER_PID=""
    stop_envd
    exit "${rc}"
fi

# envd exited while the user command (normally PostgreSQL) was still running.
# Preserve a non-zero envd status and make an unexpected clean exit fail too.
wait_for_child "${ENVD_PID}"
envd_rc=${WAIT_STATUS}
ENVD_PID=""
if [ "${envd_rc}" -eq 0 ]; then
    envd_rc=1
fi
echo "cube-entrypoint: envd exited unexpectedly with status ${envd_rc}" >&2

kill -TERM "${USER_PID}" 2>/dev/null || true
wait_for_child "${USER_PID}"
USER_PID=""
exit "${envd_rc}"
