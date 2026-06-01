#!/usr/bin/env bash
# CubeAPI curl smoke test script
#
# Usage:
#   ./test-cube-api.sh
#   CUBE_API_BASE=http://127.0.0.1:3000 ./test-cube-api.sh
#   CUBE_API_BASE=http://127.0.0.1:3001 CREATE_SANDBOX=1 TEMPLATE_ID=wecom-ds-openclaw ./test-cube-api.sh
#   CUBE_API_BASE=http://127.0.0.1:3001 CREATE_AGENT=1 ./test-cube-api.sh
#
# Env vars:
#   CUBE_API_BASE          Base URL, default http://127.0.0.1:3001
#   CUBE_API_PREFIX        Optional path prefix, default empty (use /cubeapi/v1 for Web UI proxy)
#   E2B_API_KEY            Auth key when AUTH is enabled, default dummy
#   TEMPLATE_ID            Template for create sandbox, default wecom-ds-openclaw
#   CREATE_SANDBOX         Set to 1 to POST /sandboxes and DELETE afterwards
#   CREATE_AGENT           Set to 1 to POST /agenthub/instances (OpenClaw)
#   AGENT_NAME             Agent name for CREATE_AGENT, default curl-test-agent
#   SKIP_E2B_ROOT          Set to 1 to skip root-level E2B routes (/health, /v2/sandboxes)
#   CURL_OPTS              Extra curl flags, e.g. "-k" for insecure TLS

set -euo pipefail

CUBE_API_BASE="${CUBE_API_BASE:-http://127.0.0.1:3001}"
CUBE_API_PREFIX="${CUBE_API_PREFIX:-/cubeapi/v1}"
E2B_API_KEY="${E2B_API_KEY:-dummy}"
TEMPLATE_ID="${TEMPLATE_ID:-wecom-ds-openclaw}"
CREATE_SANDBOX="${CREATE_SANDBOX:-0}"
CREATE_AGENT="${CREATE_AGENT:-0}"
AGENT_NAME="${AGENT_NAME:-curl-test-agent-$(date +%s)}"
SKIP_E2B_ROOT="${SKIP_E2B_ROOT:-0}"
CURL_OPTS="${CURL_OPTS:-}"

PASS=0
FAIL=0
CREATED_SANDBOX_ID=""
CREATED_AGENT_ID=""

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
bold() { printf '\033[1m%s\033[0m\n' "$*"; }

api_url() {
  local path="$1"
  local base="${CUBE_API_BASE%/}"
  local prefix="${CUBE_API_PREFIX}"
  if [[ -n "${prefix}" ]]; then
    prefix="/${prefix#/}"
    prefix="${prefix%/}"
  fi
  if [[ "${path}" == /* ]]; then
    printf '%s%s%s' "${base}" "${prefix}" "${path}"
  else
    printf '%s%s/%s' "${base}" "${prefix}" "${path}"
  fi
}

e2b_url() {
  local path="$1"
  local base="${CUBE_API_BASE%/}"
  printf '%s%s' "${base}" "${path}"
}

curl_json() {
  local method="$1"
  local url="$2"
  local data="${3:-}"
  local tmp
  tmp="$(mktemp)"
  local -a curl_args=( -sS -g )
  if [[ -n "${CURL_OPTS}" ]]; then
    # shellcheck disable=SC2206
    curl_args+=( ${CURL_OPTS} )
  fi
  local code
  if [[ -n "${data}" ]]; then
    code="$(curl "${curl_args[@]}" -o "${tmp}" -w '%{http_code}' -X "${method}" \
      -H 'Content-Type: application/json' \
      -H "X-API-Key: ${E2B_API_KEY}" \
      -d "${data}" \
      "${url}")"
  else
    code="$(curl "${curl_args[@]}" -o "${tmp}" -w '%{http_code}' -X "${method}" \
      -H "X-API-Key: ${E2B_API_KEY}" \
      "${url}")"
  fi
  RESP_CODE="${code}"
  RESP_BODY="$(cat "${tmp}")"
  rm -f "${tmp}"
}

assert_ok() {
  local name="$1"
  local expect_code="${2:-200}"
  if [[ "${RESP_CODE}" == "${expect_code}" ]]; then
    green "PASS  ${name} (${RESP_CODE})"
    PASS=$((PASS + 1))
  else
    red "FAIL  ${name} (expected ${expect_code}, got ${RESP_CODE})"
    printf '%.500s\n' "${RESP_BODY}"
    FAIL=$((FAIL + 1))
  fi
}

section() {
  echo
  bold "== $* =="
}

section "Config"
echo "CUBE_API_BASE=${CUBE_API_BASE}"
echo "CUBE_API_PREFIX=${CUBE_API_PREFIX}"
echo "TEMPLATE_ID=${TEMPLATE_ID}"
echo "CREATE_SANDBOX=${CREATE_SANDBOX}"
echo "CREATE_AGENT=${CREATE_AGENT}"

if [[ "${SKIP_E2B_ROOT}" != "1" ]]; then
  section "E2B root routes"
  curl_json GET "$(e2b_url /health)"
  assert_ok "GET /health"
  echo "${RESP_BODY}"

  curl_json GET "$(e2b_url /v2/sandboxes)"
  assert_ok "GET /v2/sandboxes"
  printf '%.500s\n' "${RESP_BODY}"

  curl_json GET "$(e2b_url /templates)"
  assert_ok "GET /templates"
  printf '%.500s\n' "${RESP_BODY}"
fi

section "CubeAPI /cubeapi/v1 routes"

curl_json GET "$(api_url /health)"
assert_ok "GET /cubeapi/v1/health"
echo "${RESP_BODY}"

curl_json GET "$(api_url /config)"
assert_ok "GET /cubeapi/v1/config"
echo "${RESP_BODY}"

curl_json GET "$(api_url /cluster/overview)"
assert_ok "GET /cubeapi/v1/cluster/overview"
echo "${RESP_BODY}"

curl_json GET "$(api_url /nodes)"
assert_ok "GET /cubeapi/v1/nodes"
printf '%.800s\n' "${RESP_BODY}"

curl_json GET "$(api_url /templates)"
assert_ok "GET /cubeapi/v1/templates"
printf '%.800s\n' "${RESP_BODY}"

curl_json GET "$(api_url /v2/sandboxes)"
assert_ok "GET /cubeapi/v1/v2/sandboxes"
printf '%.500s\n' "${RESP_BODY}"

curl_json GET "$(api_url /agenthub/instances)"
assert_ok "GET /cubeapi/v1/agenthub/instances"
printf '%.800s\n' "${RESP_BODY}"

curl_json GET "$(api_url /store/meta)"
assert_ok "GET /cubeapi/v1/store/meta"
printf '%.500s\n' "${RESP_BODY}"

if [[ "${CREATE_SANDBOX}" == "1" ]]; then
  section "Create sandbox (optional)"
  payload="$(cat <<EOF
{
  "templateID": "${TEMPLATE_ID}",
  "timeout": 300,
  "autoPause": false,
  "allow_internet_access": true
}
EOF
)"
  curl_json POST "$(api_url /sandboxes)" "${payload}"
  assert_ok "POST /cubeapi/v1/sandboxes" "200"
  echo "${RESP_BODY}"
  CREATED_SANDBOX_ID="$(printf '%s' "${RESP_BODY}" | sed -n 's/.*"sandboxID"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  if [[ -z "${CREATED_SANDBOX_ID}" ]]; then
    CREATED_SANDBOX_ID="$(printf '%s' "${RESP_BODY}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sandboxID",""))' 2>/dev/null || true)"
  fi
  if [[ -n "${CREATED_SANDBOX_ID}" ]]; then
    curl_json GET "$(api_url "/sandboxes/${CREATED_SANDBOX_ID}")"
    assert_ok "GET /sandboxes/${CREATED_SANDBOX_ID}"
    printf '%.500s\n' "${RESP_BODY}"

    curl_json POST "$(api_url "/sandboxes/${CREATED_SANDBOX_ID}/pause")"
    assert_ok "POST /sandboxes/${CREATED_SANDBOX_ID}/pause"
    printf '%.300s\n' "${RESP_BODY}"

    curl_json POST "$(api_url "/sandboxes/${CREATED_SANDBOX_ID}/connect")"
    assert_ok "POST /sandboxes/${CREATED_SANDBOX_ID}/connect"
    printf '%.300s\n' "${RESP_BODY}"

    curl_json DELETE "$(api_url "/sandboxes/${CREATED_SANDBOX_ID}")"
    assert_ok "DELETE /sandboxes/${CREATED_SANDBOX_ID}"
    echo "${RESP_BODY}"
  else
    red "WARN  could not parse sandboxID from create response"
  fi
fi

if [[ "${CREATE_AGENT}" == "1" ]]; then
  section "Create AgentHub instance (optional)"
  payload="$(cat <<EOF
{
  "name": "${AGENT_NAME}",
  "engine": "openclaw",
  "model": "deepseek/deepseek-v4-flash"
}
EOF
)"
  curl_json POST "$(api_url /agenthub/instances)" "${payload}"
  assert_ok "POST /cubeapi/v1/agenthub/instances" "201"
  printf '%.1200s\n' "${RESP_BODY}"
  CREATED_AGENT_ID="$(printf '%s' "${RESP_BODY}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)"
  if [[ -n "${CREATED_AGENT_ID}" ]]; then
    curl_json GET "$(api_url "/agenthub/instances/${CREATED_AGENT_ID}/wecom")"
    assert_ok "GET /agenthub/instances/${CREATED_AGENT_ID}/wecom"
    echo "${RESP_BODY}"
    echo
    echo "Created agent id: ${CREATED_AGENT_ID} (not auto-deleted; delete manually if needed)"
  fi
fi

section "Summary"
echo "Passed: ${PASS}"
echo "Failed: ${FAIL}"
if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi
