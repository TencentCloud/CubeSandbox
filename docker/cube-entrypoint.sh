#!/bin/sh
# CubeSandbox base image entrypoint.
#
# Contract:
#   1. Always start envd in the background on ${ENVD_PORT:-49983} so that
#      CubeMaster's readiness probe (:49983/health) passes within ~1s.
#   2. If a user CMD is provided (i.e. $# > 0), exec it as the foreground
#      process; envd stays alive in the background. On SIGTERM/SIGINT we
#      forward the signal to the user process; envd is reaped by tini.
#   3. If no CMD is provided, wait on envd as the foreground process so the
#      container stays up as a pure envd sandbox.
#
# Environment variables:
#   ENVD_PORT       Port envd listens on (default: 49983).
#   ENVD_EXTRA_ARGS Extra flags appended to the envd invocation (optional).
#   ENVD_LOG_FILE   Where to redirect envd stdout/stderr (default:
#                   /var/log/envd.log). Set to "-" to inherit the container
#                   stdio.

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

start_envd

if [ "$#" -eq 0 ]; then
    # No user command: keep envd as the foreground process. tini is PID 1,
    # so we simply wait for envd to exit (or be signalled).
    wait "${ENVD_PID}"
    exit $?
fi

# User command provided: forward termination signals so the user process can
# shut down cleanly; envd will be reaped by tini when the container stops.
USER_PID=""
forward_signal() {
    sig="$1"
    if [ -n "${USER_PID}" ]; then
        kill -s "${sig}" "${USER_PID}" 2>/dev/null || true
    fi
}

trap 'forward_signal TERM' TERM
trap 'forward_signal INT'  INT
trap 'forward_signal HUP'  HUP

"$@" &
USER_PID=$!
echo "cube-entrypoint: exec user command (pid=${USER_PID}): $*" >&2

# Wait on the user command; propagate its exit status.
set +e
wait "${USER_PID}"
rc=$?
set -e
exit "${rc}"
