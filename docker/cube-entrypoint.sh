#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Also support callers that explicitly invoke the former sh entrypoint.
[ -n "${BASH_VERSION:-}" ] || exec /bin/bash "$0" "$@"
# CubeSandbox image entrypoint. Runtime/container teardown owns descendants;
# with a user CMD, this entrypoint only forwards signals to that application.
set -u
# Asynchronous commands must inherit default SIGINT handling, not SIG_IGN.
set -m

ENVD_BIN=${ENVD_BIN:-/usr/bin/envd}
# Reject a second known daemon startup command; files merely present elsewhere
# in the rootfs do not imply a startup contract.
known_envd_command() {
    local -a command=("$@")
    while (( ${#command[@]} )); do
        case ${command[0]##*/} in
            tini)
                while (( ${#command[@]} > 1 )) && [[ ${command[1]} != -- ]]; do
                    command=("${command[@]:1}")
                done
                command=("${command[@]:2}")
                ;;
            sh|bash)
                if [[ ${command[1]:-} == -c ]]; then
                    read -r -a command <<< "${command[2]:-}"
                else
                    command=("${command[@]:1}")
                fi
                ;;
            exec) command=("${command[@]:1}") ;;
            envd|cube-envd|cube-entrypoint.sh|cube-envd-supervisor.sh) return 0 ;;
            *) [[ ${command[0]} == "$ENVD_BIN" ]]; return $? ;;
        esac
    done
    return 1
}
if known_envd_command "$@"; then
    echo "cube-entrypoint: user command already starts envd" >&2
    exit 1
fi
ENVD_PORT=${ENVD_PORT:-49983}
ENVD_LOG_FILE=${ENVD_LOG_FILE:-/var/log/envd.log}
# Extra arguments are whitespace-separated words, not shell code.
read -r -d '' -a envd_args <<< "${ENVD_EXTRA_ARGS:-}" || true
# Preserve the Go entrypoint's automatic -isnotfc argument and its ordering.
case " ${ENVD_EXTRA_ARGS:-} " in
    *" -isnotfc "*) ;;
    *) envd_args+=(-isnotfc) ;;
esac
if [[ ! -x $ENVD_BIN ]]; then
    echo "cube-entrypoint: envd is not executable: $ENVD_BIN" >&2
    exit 127
fi
if [[ $ENVD_LOG_FILE != - ]]; then
    mkdir -p "$(dirname "$ENVD_LOG_FILE")" || exit 1
    exec 3>>"$ENVD_LOG_FILE" || exit 1
else
    exec 3>&1
fi

"$ENVD_BIN" -port "$ENVD_PORT" "${envd_args[@]}" >&3 2>&1 3>&- &
envd_pid=$!
exec 3>&-
if (( $# == 0 )); then
    # Job control is needed only at launch for SIGINT inheritance. Disable it
    # while waiting so stopped jobs cannot supply stale termination statuses.
    set +m
    wait -f "$envd_pid"
    exit $?
fi

# Queue signals during launch so they cannot accidentally target envd's PID.
user_pid=''
pending_signals=()
forward_signal() {
    interrupted=1
    if [[ -n $user_pid ]]; then
        kill -s "$1" "$user_pid" 2>/dev/null || true
    else
        pending_signals+=("$1")
    fi
}
trap 'forward_signal TERM' TERM
trap 'forward_signal INT' INT
trap 'forward_signal HUP' HUP
"$@" &
user_pid=$!
set +m
for pending_signal in "${pending_signals[@]}"; do
    forward_signal "$pending_signal"
done

# A trap interrupts wait even if the application is still running. Re-wait on
# that same child to collect its real status, including legitimate 128+signal
# statuses. Bash retains the child's status for repeated waits by PID.
while true; do
    interrupted=0
    wait -f "$user_pid"
    status=$?
    (( interrupted )) || break
done
exit "$status"
