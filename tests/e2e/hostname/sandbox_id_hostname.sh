#!/usr/bin/env bash
# E2E: guest hostname must be the first 12 characters of the sandbox ID
# (same truncation as cubecli ls CONTAINER/CUBEBOX columns).
#
# Covers the from-template create path (and optional pause/resume). The bug this
# guards against is a memory-restore leaving the template-build UTS hostname
# (often a tpl-* prefix) in place, so `hostname` / cubecli exec prompts show
# tpl-xxxx instead of the instance short ID.
#
# Requirements: a reachable CubeAPI and a READY template on the cluster.
#
#   CUBE_API_URL=http://127.0.0.1:3000 \
#   CUBE_E2E_TEMPLATE=tpl-xxxxxxxx \
#   tests/e2e/hostname/sandbox_id_hostname.sh
#
# Optional:
#   CUBE_E2E_PAUSE_RESUME=1   also pause+resume and re-check hostname
#   CUBECLI=/path/to/cubecli  defaults to cubecli on PATH
#   CUBEMASTERCLI=...         used only to resolve SANDBOX_IP when needed
set -euo pipefail

API_URL="${CUBE_API_URL:?set CUBE_API_URL to the CubeAPI base URL}"
TEMPLATE_ID="${CUBE_E2E_TEMPLATE:?set CUBE_E2E_TEMPLATE to a READY template id}"
CUBECLI="${CUBECLI:-cubecli}"
CUBEMASTERCLI="${CUBEMASTERCLI:-cubemastercli}"
PAUSE_RESUME="${CUBE_E2E_PAUSE_RESUME:-0}"
TIMEOUT_SEC="${CUBE_E2E_TIMEOUT_SEC:-300}"

log() { printf '[hostname-e2e] %s\n' "$*"; }
die() { printf '[hostname-e2e] ERROR: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

need_cmd curl
need_cmd python3
need_cmd "$CUBECLI"

create_sandbox() {
  curl -fsS -X POST "${API_URL%/}/sandboxes" \
    -H 'Content-Type: application/json' \
    -d "{\"templateID\":\"${TEMPLATE_ID}\",\"timeout\":${TIMEOUT_SEC}}"
}

wait_running() {
  local sid="$1" state="" i
  for i in $(seq 1 60); do
    state="$(curl -fsS "${API_URL%/}/sandboxes/${sid}" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("state",""))')"
    log "wait running: t=${i} state=${state}"
    [[ "${state}" == "running" ]] && return 0
    sleep 2
  done
  die "sandbox ${sid} not running (last state=${state})"
}

sandbox_ip() {
  local sid="$1"
  if command -v "$CUBEMASTERCLI" >/dev/null 2>&1; then
    "$CUBEMASTERCLI" -a 127.0.0.1 info --sandboxid "$sid" 2>/dev/null \
      | awk '/SANDBOX_IP/{print $2; exit}'
    return 0
  fi
  return 1
}

# Prefer reading /proc hostname via a guest file (cubecli exec stdout is not
# always reliable across builds); fall back to envd /files when IP is known.
read_guest_hostname() {
  local sid="$1" ip="$2" tmp="/tmp/hostname-e2e-$$"
  "$CUBECLI" exec "$sid" /bin/sh -c "cat /proc/sys/kernel/hostname > ${tmp}" >/dev/null 2>&1 || true
  if [[ -n "${ip}" ]]; then
    curl -fsS --get "http://${ip}:49983/files" --data-urlencode "path=${tmp}" 2>/dev/null \
      | tr -d '\r\n' || true
  fi
}

assert_hostname() {
  local sid="$1" phase="$2"
  local expect="${sid:0:12}" ip hn
  ip="$(sandbox_ip "$sid" || true)"
  hn="$(read_guest_hostname "$sid" "${ip}")"
  log "${phase}: sandbox=${sid} expect=${expect} got=${hn:-<empty>} ip=${ip:-unknown}"
  [[ -n "${hn}" ]] || die "${phase}: empty guest hostname"
  [[ "${hn}" == "${expect}" ]] || die "${phase}: hostname mismatch want=${expect} got=${hn}"
  log "${phase}: PASS"
}

destroy_sandbox() {
  local sid="$1"
  curl -fsS -X DELETE "${API_URL%/}/sandboxes/${sid}" >/dev/null 2>&1 || true
}

# --- main ---
log "api=${API_URL} template=${TEMPLATE_ID}"
RESP="$(create_sandbox)"
SID="$(printf '%s' "$RESP" | python3 -c 'import sys,json; print(json.load(sys.stdin)["sandboxID"])')"
[[ -n "${SID}" ]] || die "create returned no sandboxID: ${RESP}"
trap 'destroy_sandbox "${SID}"' EXIT

wait_running "$SID"
assert_hostname "$SID" "create"

if [[ "${PAUSE_RESUME}" == "1" ]]; then
  log "pause ${SID}"
  curl -fsS -X POST "${API_URL%/}/sandboxes/${SID}/pause" -H 'Content-Type: application/json' -d '{}' >/dev/null
  for i in $(seq 1 30); do
    st="$(curl -fsS "${API_URL%/}/sandboxes/${SID}" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("state",""))')"
    [[ "${st}" == "paused" ]] && break
    sleep 1
  done
  log "resume ${SID}"
  curl -fsS -X POST "${API_URL%/}/sandboxes/${SID}/resume" \
    -H 'Content-Type: application/json' -d "{\"timeout\":${TIMEOUT_SEC}}" >/dev/null
  wait_running "$SID"
  assert_hostname "$SID" "after-pause-resume"
fi

log "ALL PASS"
