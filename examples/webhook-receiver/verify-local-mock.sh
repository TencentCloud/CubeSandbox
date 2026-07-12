#!/usr/bin/env bash
# Runs CubeAPI lifecycle webhook verification without a CubeSandbox compute node.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
API_BIN="${CUBE_API_BIN:-${ROOT_DIR}/CubeAPI/target/debug/cube-api}"
RUN_DIR="${WEBHOOK_E2E_LOG_DIR:-$(mktemp -d /tmp/cubesandbox-webhook-e2e.XXXXXX)}"
API_PORT="${WEBHOOK_E2E_API_PORT:-13000}"
MOCK_PORT="${WEBHOOK_E2E_MOCK_PORT:-18089}"
RECEIVER_PORT="${WEBHOOK_E2E_RECEIVER_PORT:-9000}"

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing required command: $1" >&2
        exit 1
    }
}

cleanup() {
    kill "${API_PID:-}" "${RECEIVER_PID:-}" "${MOCK_PID:-}" 2>/dev/null || true
    wait "${API_PID:-}" "${RECEIVER_PID:-}" "${MOCK_PID:-}" 2>/dev/null || true
}
trap cleanup EXIT

require_command curl
require_command python3
[[ -x "${API_BIN}" ]] || {
    echo "CubeAPI binary not found: ${API_BIN}" >&2
    echo "Build it first, for example: (cd CubeAPI && cargo build)" >&2
    exit 1
}

mkdir -p "${RUN_DIR}"
wait_for_http() {
    local url="$1"

    for _ in $(seq 1 50); do
        curl -fsS "${url}" >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    echo "service did not become ready: ${url}" >&2
    return 1
}

python3 "${SCRIPT_DIR}/cubemaster-mock.py" --port "${MOCK_PORT}" >"${RUN_DIR}/cubemaster-mock.log" 2>&1 &
MOCK_PID=$!
python3 "${SCRIPT_DIR}/receiver.py" --port "${RECEIVER_PORT}" --secret change-me >"${RUN_DIR}/receiver.log" 2>&1 &
RECEIVER_PID=$!
wait_for_http "http://127.0.0.1:${MOCK_PORT}/health"
wait_for_http "http://127.0.0.1:${RECEIVER_PORT}/health"

env \
    CUBE_API_BIND="127.0.0.1:${API_PORT}" \
    CUBE_API_SANDBOX_DOMAIN=cube.local \
    CUBE_MASTER_ADDR="http://127.0.0.1:${MOCK_PORT}" \
    LOG_DIR="${RUN_DIR}/cube-api-log" \
    WEBHOOK__ENABLED=true \
    WEBHOOK__ENDPOINTS__0__NAME=local-receiver \
    WEBHOOK__ENDPOINTS__0__URL="http://127.0.0.1:${RECEIVER_PORT}/webhook" \
    WEBHOOK__ENDPOINTS__0__EVENTS__0=sandbox.created \
    WEBHOOK__ENDPOINTS__0__EVENTS__1=sandbox.deleted \
    WEBHOOK__ENDPOINTS__0__EVENTS__2=sandbox.paused \
    WEBHOOK__ENDPOINTS__0__EVENTS__3=sandbox.resumed \
    WEBHOOK__ENDPOINTS__0__SECRET=change-me \
    NO_PROXY=127.0.0.1,localhost \
    "${API_BIN}" >"${RUN_DIR}/cube-api.log" 2>&1 &
API_PID=$!

wait_for_http "http://127.0.0.1:${API_PORT}/health"

create="$(curl -fsS -X POST "http://127.0.0.1:${API_PORT}/sandboxes" \
    -H 'Content-Type: application/json' -d '{"templateID":"tpl-local","timeout":120}')"
sandbox_id="$(printf '%s' "${create}" | sed -n 's/.*"sandboxID":"\([^"]*\)".*/\1/p')"
[[ "${sandbox_id}" == "mock-sandbox-1" ]]
printf 'CREATE 200 %s\n' "${create}"
curl -fsS -o /dev/null -w 'PAUSE %{http_code}\n' -X POST "http://127.0.0.1:${API_PORT}/sandboxes/${sandbox_id}/pause"
curl -fsS -o /dev/null -w 'RESUME %{http_code}\n' -X POST "http://127.0.0.1:${API_PORT}/sandboxes/${sandbox_id}/resume" \
    -H 'Content-Type: application/json' -d '{"timeout":120,"autoPause":false}'
curl -fsS -o /dev/null -w 'DELETE %{http_code}\n' -X DELETE "http://127.0.0.1:${API_PORT}/sandboxes/${sandbox_id}"

for _ in $(seq 1 30); do
    [[ "$(grep -c '"event"' "${RUN_DIR}/receiver.log" || true)" -eq 4 ]] && break
    sleep 0.1
done
[[ "$(grep -c '"event"' "${RUN_DIR}/receiver.log" || true)" -eq 4 ]]

echo
echo "--- Receiver output ---"
cat "${RUN_DIR}/receiver.log"
echo
echo "Logs saved to: ${RUN_DIR}"
