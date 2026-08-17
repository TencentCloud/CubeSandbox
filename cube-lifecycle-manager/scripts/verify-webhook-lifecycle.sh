#!/usr/bin/env bash
# End-to-end verification of CLM webhook delivery against a live deployment.
#
# It starts the example receiver and a cube-lifecycle-manager configured with
# CUBE_LCM_WEBHOOK_ENDPOINTS, drives a sandbox through create → pause → resume
# → delete via the CubeAPI HTTP API, and asserts the receiver logs all four
# events with the expected payload fields.
#
# Required environment:
#   CLM_BINARY         path to the cube-lifecycle-manager binary to test
#   CLM_ENV            env file with the CLM baseline configuration for the
#                      deployment (CUBE_LCM_REDIS_ADDR, CUBE_LCM_CUBEMASTER_URL,
#                      CUBE_LCM_PROXY_ADMIN_URLS, CUBE_LCM_USE_STATIC_FLEET, ...)
#   CUBE_API_URL       base URL of a running CubeAPI (e.g. http://127.0.0.1:3000)
#   WEBHOOK_RECEIVER   path to examples/webhook-receiver/receiver.py
#
# Optional:
#   WEBHOOK_TEST_DIR   scratch dir for logs      (default /tmp/clm-webhook-test)
#   WEBHOOK_TEST_TEMPLATE_ID                     (default wecom-ds-openclaw)
#   PYTHON             python interpreter        (default python3)
set -euo pipefail

: "${CLM_BINARY:?set CLM_BINARY to the cube-lifecycle-manager binary}"
: "${CLM_ENV:?set CLM_ENV to the CLM environment file}"
: "${CUBE_API_URL:?set CUBE_API_URL to a running CubeAPI base URL}"
: "${WEBHOOK_RECEIVER:?set WEBHOOK_RECEIVER to receiver.py}"

TEST_DIR="${WEBHOOK_TEST_DIR:-/tmp/clm-webhook-test}"
TEMPLATE_ID="${WEBHOOK_TEST_TEMPLATE_ID:-wecom-ds-openclaw}"
PYTHON="${PYTHON:-python3}"
CLM_PID=""
RECEIVER_PID=""

cleanup() {
  [[ -n "${CLM_PID}" ]] && kill "${CLM_PID}" 2>/dev/null || true
  [[ -n "${RECEIVER_PID}" ]] && kill "${RECEIVER_PID}" 2>/dev/null || true
}
trap cleanup EXIT

rm -rf "${TEST_DIR}"
mkdir -p "${TEST_DIR}"

# 1. Receiver with HMAC verification + replay protection enabled.
export CUBE_WEBHOOK_SECRET="cube-webhook-integration-test"
export PORT=9010
PYTHONUNBUFFERED=1 "${PYTHON}" "${WEBHOOK_RECEIVER}" >"${TEST_DIR}/receiver.log" 2>&1 &
RECEIVER_PID=$!

# 2. CLM with the webhook subscription overlaid on the deployment config.
set -a
source "${CLM_ENV}"
set +a
export CUBE_LCM_LISTEN_ADDR="127.0.0.1:8083"
export CUBE_LCM_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:9010/webhooks/cube","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"cube-webhook-integration-test"}]'
"${CLM_BINARY}" >"${TEST_DIR}/clm.log" 2>&1 &
CLM_PID=$!

for _ in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8083/readyz >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://127.0.0.1:8083/readyz >/dev/null

# 3. Drive one sandbox through its full lifecycle.
create_body=$(printf '{"templateID":"%s","timeout":300,"allow_internet_access":true}' "${TEMPLATE_ID}")
create_response=$(curl -fsS -X POST "${CUBE_API_URL}/sandboxes" -H 'content-type: application/json' --data "${create_body}")
printf '%s\n' "${create_response}" >"${TEST_DIR}/create-response.json"
sandbox_id=$(printf '%s' "${create_response}" | "${PYTHON}" -c 'import json,sys; print(json.load(sys.stdin)["sandboxID"])')

curl -fsS -o /dev/null -w '%{http_code}' -X POST "${CUBE_API_URL}/sandboxes/${sandbox_id}/pause" | grep -qx '204'
curl -fsS -o /dev/null -w '%{http_code}' -X POST "${CUBE_API_URL}/sandboxes/${sandbox_id}/resume" -H 'content-type: application/json' --data '{}' | grep -qx '201'
curl -fsS -o /dev/null -w '%{http_code}' -X DELETE "${CUBE_API_URL}/sandboxes/${sandbox_id}" | grep -qx '204'

# 4. The events flow CubeMaster -> Redis stream -> CLM -> receiver, so allow
#    for propagation time before giving up.
for _ in $(seq 1 30); do
  if "${PYTHON}" - "${TEST_DIR}/receiver.log" "${sandbox_id}" "${TEMPLATE_ID}" <<'PY'
from datetime import datetime
import json
import sys

path, sandbox_id, template_id = sys.argv[1:]
expected = {"sandbox.created", "sandbox.paused", "sandbox.resumed", "sandbox.deleted"}
received = {}
for line in open(path, encoding="utf-8"):
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        continue
    if event.get("sandbox_id") == sandbox_id:
        received[event.get("event")] = event
missing = expected - received.keys()
if missing:
    raise SystemExit("missing events: " + ", ".join(sorted(missing)))
for name in expected:
    event = received[name]
    if event.get("version") != "1":
        raise SystemExit(f"{name}: expected version='1', got {event.get('version')!r}")
    timestamp = event.get("timestamp")
    if not isinstance(timestamp, str):
        raise SystemExit(f"{name}: missing timestamp")
    try:
        datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
    except ValueError as error:
        raise SystemExit(f"{name}: invalid timestamp {timestamp!r}: {error}")
created = received["sandbox.created"]
if created.get("template_id") != template_id:
    raise SystemExit(
        f"sandbox.created: expected template_id={template_id!r}, "
        f"got {created.get('template_id')!r}"
    )
for name, state in (("sandbox.paused", "paused"), ("sandbox.resumed", "running")):
    if received[name].get("state") != state:
        raise SystemExit(
            f"{name}: expected state={state!r}, got {received[name].get('state')!r}"
        )
PY
  then
    echo "Webhook lifecycle verification passed for ${sandbox_id}"
    exit 0
  fi
  sleep 1
done

echo "Webhook event verification timed out; inspect ${TEST_DIR}" >&2
exit 1
