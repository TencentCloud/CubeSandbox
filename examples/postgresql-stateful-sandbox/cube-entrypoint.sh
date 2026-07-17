#!/bin/sh
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

# Start envd beside the foreground PostgreSQL process and wait for PostgreSQL
# to finish shutting down before the container exits. The generic entrypoint in
# cubesandbox-base forwards stop signals, but its interrupted wait can return
# before PostgreSQL has completed its fast shutdown.

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

stop_envd() {
    if kill -0 "${ENVD_PID}" 2>/dev/null; then
        kill -TERM "${ENVD_PID}" 2>/dev/null || true
    fi
    wait "${ENVD_PID}" 2>/dev/null || true
}

start_envd

if [ "$#" -eq 0 ]; then
    trap 'stop_envd; exit 0' TERM INT
    wait "${ENVD_PID}"
    exit $?
fi

USER_PID=""

# Invoked by the TERM/INT traps below.
# shellcheck disable=SC2329
shutdown() {
    signal="$1"
    trap - TERM INT HUP

    if [ -n "${USER_PID}" ] && kill -0 "${USER_PID}" 2>/dev/null; then
        kill -s "${signal}" "${USER_PID}" 2>/dev/null || true
        set +e
        wait "${USER_PID}"
        rc=$?
        set -e
    else
        rc=0
    fi

    stop_envd
    exit "${rc}"
}

# Invoked by the HUP trap below.
# shellcheck disable=SC2329
forward_hup() {
    if [ -n "${USER_PID}" ]; then
        kill -HUP "${USER_PID}" 2>/dev/null || true
    fi
}

trap 'shutdown TERM' TERM
trap 'shutdown INT' INT
trap 'forward_hup' HUP

"$@" &
USER_PID=$!
echo "cube-entrypoint: exec user command (pid=${USER_PID}): $*" >&2

# A non-terminating signal such as HUP interrupts wait(1). Keep waiting while
# the PostgreSQL process is still alive instead of treating that interruption
# as the process exit status.
while true; do
    set +e
    wait "${USER_PID}"
    rc=$?
    set -e
    if kill -0 "${USER_PID}" 2>/dev/null; then
        continue
    fi
    break
done

stop_envd
exit "${rc}"
