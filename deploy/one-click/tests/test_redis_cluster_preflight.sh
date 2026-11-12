#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval "$(sed -n '/^check_external_redis_cluster_connectivity() {$/,/^}$/p' "${ROOT}/install.sh")"
log() { :; }
die() { echo "$*" >&2; exit 1; }
redis_cli_help_output() { echo '--connect-timeout --timeout'; }
redis_cli_help_supports_flag() { [[ "$1" == *"$2"* ]]; }
run_redis_preflight_cmd() { shift 2; "$@"; }
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
redis-cli() {
  local args=" $* "
  printf '%s\n' "${args}" >> "${TMP}/calls"
  if [[ "${args}" == *' AUTH '* ]]; then
    local password
    password="$(cat)"
    [[ "${password}" == fixture-secret ]] || return 1
    if [[ "${SCENARIO}" == bad-auth ]]; then echo WRONGPASS; else echo OK; fi
  elif [[ "${args}" == *' CLUSTER INFO '* ]]; then
    [[ "${REDISCLI_AUTH}" == fixture-secret ]] || return 1
    if [[ "${SCENARIO}" == unhealthy ]]; then echo cluster_state:fail; else printf 'cluster_state:ok\r\n'; fi
  else
    return 1
  fi
}
CUBE_EXTERNAL_REDIS_CLUSTER_NODES='redis-a.example:6379, redis-b.example:6380'
CUBE_EXTERNAL_REDIS_PASSWORD=fixture-secret
SCENARIO=healthy
check_external_redis_cluster_connectivity
[[ $(wc -l < "${TMP}/calls") -eq 4 ]]
grep -q 'redis-a.example -p 6379' "${TMP}/calls"
grep -q 'redis-b.example -p 6380' "${TMP}/calls"
! grep -q fixture-secret "${TMP}/calls"
for SCENARIO in bad-auth unhealthy; do
  if (check_external_redis_cluster_connectivity) >/dev/null 2>&1; then
    die "Unexpectedly accepted ${SCENARIO}"
  fi
done
echo 'Redis Cluster preflight tests passed'
