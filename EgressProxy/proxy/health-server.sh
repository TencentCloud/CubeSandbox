#!/bin/sh
set -eu

LISTEN_IP="${1:?listen IP is required}"
HEALTH_PORT=19091

while true; do
  if /opt/cube-egress/health.sh; then
    response=ok
  else
    response=failed
  fi
  printf '%s\n' "${response}" |
    nc -l -s "${LISTEN_IP}" -p "${HEALTH_PORT}"
done
