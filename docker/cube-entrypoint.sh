#!/bin/sh
# CubeSandbox base image entrypoint.
#
# Contract:
#   1. Always start envd in the background on ${ENVD_PORT:-49983} so that
#      CubeMaster's readiness probe (:49983/health) passes within ~1s.
#   2. If a user CMD is provided (i.e. $# > 0), run it as the foreground
#      workload while monitoring envd. An unexpected envd exit is logged and
#      terminates the user process.
#   3. If no CMD is provided, wait on envd as the foreground process so the
#      container stays up as a pure envd sandbox.
#
# Environment variables:
#   ENVD_PORT       Port envd listens on (default: 49983).
#   ENVD_EXTRA_ARGS Extra flags appended to the envd invocation (default: empty).
#                   -isnotfc is appended automatically if not already present.
#   ENVD_LOG_FILE   Where to redirect envd stdout/stderr (default:
#                   /var/log/envd.log). Set to "-" to inherit the container
#                   stdio.
#   ENVD_LOG_LEVEL  Maximum tracing level: error, warn, info, debug, or trace
#                   (default: info).
#   ENVD_LOG_FORMAT Log format: pretty or json (default: pretty).

set -eu

ENVD_BIN="${ENVD_BIN:-/usr/bin/envd}"
ENVD_PORT="${ENVD_PORT:-49983}"
ENVD_LOG_FILE="${ENVD_LOG_FILE:-/var/log/envd.log}"
ENVD_EXTRA_ARGS="${ENVD_EXTRA_ARGS:-}"
case " ${ENVD_EXTRA_ARGS} " in
    *" -isnotfc "*) ;;
    *) ENVD_EXTRA_ARGS="${ENVD_EXTRA_ARGS} -isnotfc" ;;
esac

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

USER_PID=""
SHUTTING_DOWN=0

process_is_running() {
    pid="$1"
    [ -r "/proc/${pid}/stat" ] || return 1
    stat="$(cat "/proc/${pid}/stat" 2>/dev/null || true)"
    [ -n "${stat}" ] || return 1
    # Field 2 (comm) is wrapped in parens and may itself contain spaces or
    # parens, so resume parsing after the last ')': the next field is state.
    rest="${stat##*) }"
    state="${rest%% *}"
    [ "${state:-}" != "Z" ]
}

stop_envd() {
    if [ -n "${ENVD_PID}" ]; then
        kill -s TERM "${ENVD_PID}" 2>/dev/null || true
    fi
}

forward_signal() {
    sig="$1"
    SHUTTING_DOWN=1
    stop_envd
    if [ -n "${USER_PID}" ]; then
        kill -s "${sig}" "${USER_PID}" 2>/dev/null || true
    fi
}

trap 'forward_signal TERM' TERM
trap 'forward_signal INT' INT
trap 'forward_signal HUP' HUP

if [ "$#" -eq 0 ]; then
    set +e
    wait "${ENVD_PID}"
    rc=$?
    set -e
    if [ "${SHUTTING_DOWN}" -eq 0 ]; then
        echo "cube-entrypoint: envd exited unexpectedly (pid=${ENVD_PID}, exit_code=${rc})" >&2
    fi
    exit "${rc}"
fi

"$@" &
USER_PID=$!
echo "cube-entrypoint: exec user command (pid=${USER_PID}): $*" >&2

while process_is_running "${ENVD_PID}"; do
    if ! process_is_running "${USER_PID}"; then
        set +e
        wait "${USER_PID}"
        user_rc=$?
        set -e
        SHUTTING_DOWN=1
        stop_envd
        set +e
        wait "${ENVD_PID}"
        set -e
        exit "${user_rc}"
    fi
    sleep 1
done

set +e
wait "${ENVD_PID}"
envd_rc=$?
set -e
if [ "${SHUTTING_DOWN}" -eq 0 ]; then
    echo "cube-entrypoint: envd exited unexpectedly (pid=${ENVD_PID}, exit_code=${envd_rc})" >&2
    kill -s TERM "${USER_PID}" 2>/dev/null || true
fi
wait "${USER_PID}" 2>/dev/null || true
if [ "${SHUTTING_DOWN}" -eq 0 ]; then
    exit 1
fi
exit "${envd_rc}"
