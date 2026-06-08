#!/bin/bash
# CubeProxy container entrypoint.
#
# Layout:
#   - Foreground: openresty/nginx (PID 1's main duty after exec)
#   - Background: cube-proxy-sidecar, lifecycle coordination loop
#   - Background: crond, log rotation
#
# The sidecar binary is shipped inside this image rather than as a separate
# container so the lifecycle (auto-pause / auto-resume) feature is always
# co-resident with nginx. The binary is REQUIRED — if it is missing or
# unreadable we abort the entrypoint instead of starting a half-functional
# container that quietly drops the auto-pause feature. The Dockerfile
# performs the same sanity checks at build time; this is the runtime
# belt-and-braces.

set -u

SIDECAR_BIN="${SIDECAR_BIN:-/usr/local/openresty/nginx/sbin/cube-proxy-sidecar}"
SIDECAR_LOG="${SIDECAR_LOG:-/data/log/cube-proxy/sidecar.log}"

start_sidecar() {
  if [[ ! -x "${SIDECAR_BIN}" ]]; then
    echo "$(date -Iseconds) FATAL: cube-proxy-sidecar binary missing or not executable at ${SIDECAR_BIN}" >&2
    echo "$(date -Iseconds)        rebuild the cube-proxy image (CubeProxy/Makefile prebuild-sidecar)" >&2
    return 1
  fi

  # Loop in the background so a sidecar crash auto-restarts without taking
  # nginx down with it. Exponential-ish backoff bounded at 30s.
  (
    backoff=1
    while true; do
      "${SIDECAR_BIN}" >>"${SIDECAR_LOG}" 2>&1 &
      sidecar_pid=$!
      wait "${sidecar_pid}"
      rc=$?
      echo "$(date -Iseconds) cube-proxy-sidecar exited rc=${rc}; restarting in ${backoff}s" >>"${SIDECAR_LOG}"
      sleep "${backoff}"
      if [[ "${backoff}" -lt 30 ]]; then
        backoff=$((backoff * 2))
        [[ "${backoff}" -gt 30 ]] && backoff=30
      fi
    done
  ) &
  echo "$(date -Iseconds) cube-proxy-sidecar supervisor started (logs: ${SIDECAR_LOG})" >&2
}

mkdir -p "$(dirname "${SIDECAR_LOG}")"

/usr/sbin/crond
# Abort the entrypoint if the sidecar can't be brought up — nginx alone
# would silently mishandle paused sandboxes (returning 503 forever).
start_sidecar || exit 1
exec /usr/local/openresty/nginx/sbin/nginx
