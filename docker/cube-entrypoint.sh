#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Also support callers that explicitly invoke the former sh entrypoint.
[ -n "${BASH_VERSION:-}" ] || exec /bin/bash "$0" "$@"
# CubeSandbox image supervisor. Runtime/container teardown owns descendants;
# this entrypoint only signals the two leaders it starts.
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

# A single FIFO orders child completion and external signals, including exits
# before the supervisor begins waiting. Each record is one atomic pipe write.
events=$(mktemp -d) || exit 1
mkfifo "$events/events" || { rmdir "$events"; exit 1; }
exec 4<>"$events/events"
rm -rf "$events"
trap 'printf "Signal TERM 143\n" >&4' TERM
trap 'printf "Signal INT 130\n" >&4' INT
trap 'printf "Signal HUP 129\n" >&4' HUP

monitor() {
    local role=$1 pid status
    shift
    "$@" 3>&- 4>&- &
    pid=$!
    printf 'Start %s %s\n' "$role" "$pid" >&4
    wait "$pid"
    status=$?
    printf 'Exit %s %s\n' "$role" "$status" >&4
}
monitor Envd "$ENVD_BIN" -port "$ENVD_PORT" "${envd_args[@]}" >&3 2>&1 &
envd_monitor=$!
user_monitor=
if (( $# )); then
    monitor User "$@" &
    user_monitor=$!
fi
exec 3>&-
envd_pid='' user_pid='' cause='' status=0 terminal_signal=TERM
envd_done=0 user_done=0
[[ -n $user_monitor ]] || user_done=1

# Once selected, a cause and its status never change during teardown.
while true; do
    # A trapped signal can interrupt read after its handler queues the event.
    IFS=' ' read -r event role value <&4 || continue
    case $event in
        Start)
            if [[ $role == Envd ]]; then envd_pid=$value; else user_pid=$value; fi
            ;;
        Exit)
            if [[ $role == Envd ]]; then envd_done=1; else user_done=1; fi
            if [[ -z $cause ]]; then
                status=$value
                if [[ $role == User ]]; then
                    cause=UserExit
                else
                    cause=EnvdFailure
                    (( status != 0 )) || status=1
                fi
            fi
            ;;
        Signal)
            if [[ -z $cause ]]; then
                cause=ExternalSignal
                status=$value
                terminal_signal=$role
            fi
            ;;
    esac
    # Start records precede each child's Exit record; collect both leader PIDs
    # before teardown even if a signal or immediate exit arrived during startup.
    if [[ -n $cause && -n $envd_pid ]] && { [[ -z $user_monitor || -n $user_pid ]]; }; then
        break
    fi
done
trap '' TERM INT HUP
echo "cube-entrypoint: terminal cause=$cause status=$status" >&2
if (( ! envd_done )); then kill -TERM "$envd_pid" 2>/dev/null || true; fi
if (( ! user_done )); then kill -s "$terminal_signal" "$user_pid" 2>/dev/null || true; fi

# A single fixed grace bounds both leaders. Later errors/signals cannot replace
# the selected cause; no signal is sent to either leader's process group.
deadline=$((SECONDS + 5))
while (( ! envd_done || ! user_done )); do
    remaining=$((deadline - SECONDS))
    (( remaining > 0 )) || break
    IFS=' ' read -r -t "$remaining" event role value <&4 || break
    if [[ $event == Exit ]]; then
        if [[ $role == Envd ]]; then envd_done=1; else user_done=1; fi
    fi
done
(( envd_done )) || kill -KILL "$envd_pid" 2>/dev/null || true
(( user_done )) || kill -KILL "$user_pid" 2>/dev/null || true
# Do not wait after forced termination: a leader blocked in a kernel syscall
# may not reap yet. Container/runtime teardown completes that cleanup.
if (( envd_done )); then wait "$envd_monitor" 2>/dev/null || true; fi
if [[ -n $user_monitor ]] && (( user_done )); then wait "$user_monitor" 2>/dev/null || true; fi
exit "$status"
