#!/usr/bin/env bash
# Smoke-tests CubeOps delivery by injecting CubeMaster-compatible Redis events.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="${WEBHOOK_E2E_LOG_DIR:-$(mktemp -d /tmp/cubesandbox-webhook-e2e.XXXXXX)}"
CUBE_OPS_URL="${CUBE_OPS_URL:-http://127.0.0.1:3010}"
REDIS_URL="${REDIS_URL:-redis://127.0.0.1:6379/0}"
RECEIVER_PORT="${WEBHOOK_E2E_RECEIVER_PORT:-9000}"
STREAM=cube:v1:shared:sandbox:lifecycle:events

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing required command: $1" >&2
        exit 1
    }
}

cleanup() {
    kill "${RECEIVER_PID:-}" 2>/dev/null || true
    wait "${RECEIVER_PID:-}" 2>/dev/null || true
}
trap cleanup EXIT

require_command curl
require_command python3
require_command redis-cli
mkdir -p "${RUN_DIR}"

python3 "${SCRIPT_DIR}/receiver.py" --port "${RECEIVER_PORT}" --secret change-me \
    >"${RUN_DIR}/receiver.log" 2>&1 &
RECEIVER_PID=$!

for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${RECEIVER_PORT}/health" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -fsS "${CUBE_OPS_URL}/health" >/dev/null

event_id() {
    python3 -c 'import uuid; print(uuid.uuid4())'
}

now_ms() {
    python3 -c 'import time; print(time.time_ns() // 1_000_000)'
}

redis-cli -u "${REDIS_URL}" XADD "${STREAM}" '*' \
    op create sandbox_id sandbox-smoke ts "$(now_ms)" event_id "$(event_id)" \
    payload '{"sandbox_id":"sandbox-smoke","template_id":"template-smoke","instance_type":"cubebox"}' >/dev/null
redis-cli -u "${REDIS_URL}" XADD "${STREAM}" '*' \
    op state sandbox_id sandbox-smoke ts "$(now_ms)" event_id "$(event_id)" \
    payload '{"state":"paused","actor":"cubemaster","source":"smoke-test"}' >/dev/null
redis-cli -u "${REDIS_URL}" XADD "${STREAM}" '*' \
    op state sandbox_id sandbox-smoke ts "$(now_ms)" event_id "$(event_id)" \
    payload '{"state":"running","actor":"cubemaster","source":"smoke-test"}' >/dev/null
redis-cli -u "${REDIS_URL}" XADD "${STREAM}" '*' \
    op delete sandbox_id sandbox-smoke ts "$(now_ms)" event_id "$(event_id)" >/dev/null

for _ in $(seq 1 50); do
    grep -oE '"event": "sandbox\.[^"]+"' "${RUN_DIR}/receiver.log" \
        >"${RUN_DIR}/events.txt" || true
    [[ "$(wc -l <"${RUN_DIR}/events.txt")" -ge 4 ]] && break
    sleep 0.2
done

python3 - "${RUN_DIR}/events.txt" <<'PY'
import sys

expected = [
    '"event": "sandbox.created"',
    '"event": "sandbox.paused"',
    '"event": "sandbox.resumed"',
    '"event": "sandbox.deleted"',
]
actual = [line.strip() for line in open(sys.argv[1], encoding="utf-8")]
if actual != expected:
    raise SystemExit(f"Webhook events mismatch:\nexpected={expected}\nactual={actual}")
print("CubeOps Webhook smoke test: PASS")
PY

cat "${RUN_DIR}/receiver.log"
echo "Logs saved to: ${RUN_DIR}"
