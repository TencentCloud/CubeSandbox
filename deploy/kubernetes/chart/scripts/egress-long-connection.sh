#!/usr/bin/env bash

set -u

target_host="${1:?usage: $0 HOST [PORT] [INTERVAL_SECONDS] [STATE_FILE]}"
target_port="${2:-80}"
interval="${3:-1}"
state_file="${4:-/tmp/egress-long-connection.state}"

ok=0
fail=0
status=""

if ! exec 3<>"/dev/tcp/${target_host}/${target_port}"; then
    printf 'ok=%s fail=1 ts=%s status=connect_failed\n' \
        "${ok}" "$(date +%s%3N)" >"${state_file}"
    exit 1
fi

while true; do
    if ! printf 'GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n' \
        "${target_host}" >&3; then
        fail=$((fail + 1))
        break
    fi

    if ! IFS= read -r status <&3; then
        fail=$((fail + 1))
        break
    fi

    while IFS= read -r line <&3; do
        line="${line%$'\r'}"
        [[ -z "${line}" ]] && break
    done

    if ! IFS= read -r _body <&3; then
        fail=$((fail + 1))
        break
    fi

    ok=$((ok + 1))
    printf 'ok=%s fail=%s ts=%s status=%s\n' \
        "${ok}" "${fail}" "$(date +%s%3N)" "${status%$'\r'}" >"${state_file}"
    sleep "${interval}"
done

printf 'ok=%s fail=%s ts=%s status=%s\n' \
    "${ok}" "${fail}" "$(date +%s%3N)" "${status%$'\r'}" >"${state_file}"
exit 1
