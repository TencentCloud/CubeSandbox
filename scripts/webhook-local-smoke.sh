#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Webhook local smoke test: validates the CubeOps webhook pipeline end to end
# (subscription → test delivery → HMAC-signed POST → receiver → delivery
# status), reusing an existing one-click / docker-compose stack when present.
#
# Requirements:
#   - CubeOps running with webhook.enabled=true and redis_url set
#   - MySQL/Redis reachable (one-click stack or docker-compose)
#   - curl, jq, cargo
#
# Usage:
#   CUBE_OPS_ADDR=http://127.0.0.1:3010 \
#   ADMIN_USER=admin ADMIN_PASSWORD=... \
#   ./scripts/webhook-local-smoke.sh
#
# WARNING: this script enables allow_private_networks=true so the receiver on
# 127.0.0.1 is reachable. It is for LOCAL testing only — never use these
# settings in production.
set -euo pipefail

echo "WARNING: local smoke enables allow_private_networks=true (loopback receiver); DO NOT reuse in production."

log()  { echo "[smoke] $*"; }
fail() { echo "[smoke] FAIL: $*" >&2; exit 1; }

command -v curl >/dev/null || fail "curl is required"
command -v jq   >/dev/null || fail "jq is required"

CUBE_OPS_ADDR="${CUBE_OPS_ADDR:-http://127.0.0.1:3010}"
RECEIVER_URL="${RECEIVER_URL:-http://127.0.0.1:9090/webhook}"
RECEIVER_BASE="${RECEIVER_URL%/webhook}"
WEBHOOK_SECRET="${WEBHOOK_SECRET:-test-secret}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
CUBE_OPS_TOKEN="${CUBE_OPS_TOKEN:-}"

# 1. CubeOps must be up.
curl -sf "${CUBE_OPS_ADDR}/health" >/dev/null 2>&1 || fail "CubeOps not reachable at ${CUBE_OPS_ADDR}/health"
log "CubeOps reachable"

# 2. Obtain a token (or use CUBE_OPS_TOKEN).
if [[ -z "${CUBE_OPS_TOKEN}" ]]; then
  [[ -n "${ADMIN_PASSWORD}" ]] || fail "set ADMIN_PASSWORD or CUBE_OPS_TOKEN"
  LOGIN=$(curl -sf -X POST "${CUBE_OPS_ADDR}/api/v1/login" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASSWORD}\"}") \
    || fail "CubeOps login failed"
  CUBE_OPS_TOKEN=$(printf '%s' "${LOGIN}" | jq -r '.access_token // .token // empty')
  [[ -n "${CUBE_OPS_TOKEN}" ]] || fail "could not extract access token from login response"
fi
AUTH=(-H "Authorization: Bearer ${CUBE_OPS_TOKEN}")
log "authenticated"

# 3. Ensure the receiver is running (build + start when absent).
RECEIVER_PID=""
cleanup() { [[ -n "${RECEIVER_PID}" ]] && kill "${RECEIVER_PID}" 2>/dev/null || true; }
trap cleanup EXIT
if ! curl -sf "${RECEIVER_BASE}/health" >/dev/null 2>&1; then
  RECEIVER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../examples/webhook-receiver" && pwd)"
  log "starting webhook-receiver from ${RECEIVER_DIR}"
  (cd "${RECEIVER_DIR}" && WEBHOOK_SECRET="${WEBHOOK_SECRET}" cargo run >/tmp/webhook-receiver-smoke.log 2>&1) &
  RECEIVER_PID=$!
  for _ in $(seq 1 90); do
    curl -sf "${RECEIVER_BASE}/health" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -sf "${RECEIVER_BASE}/health" >/dev/null 2>&1 || fail "receiver did not start (see /tmp/webhook-receiver-smoke.log)"
fi
log "receiver reachable at ${RECEIVER_URL}"

# 4. Register a subscription (idempotent: recreate with a unique name).
NAME="smoke-$(date +%s)"
CREATED=$(curl -sf -X POST "${CUBE_OPS_ADDR}/api/v1/webhooks" "${AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"${NAME}\",\"url\":\"${RECEIVER_URL}\",\"events\":[\"sandbox.created\"],\"secret\":\"${WEBHOOK_SECRET}\"}") \
  || fail "subscription create failed"
SUB_ID=$(printf '%s' "${CREATED}" | jq -r '.id')
[[ "${SUB_ID}" =~ ^[0-9]+$ ]] || fail "invalid subscription id: ${SUB_ID}"
log "subscription ${SUB_ID} created"

# 5. Fire a test delivery and wait for the receiver + ledger status.
TEST=$(curl -sf -X POST "${CUBE_OPS_ADDR}/api/v1/webhooks/${SUB_ID}/test" "${AUTH[@]}") \
  || fail "test delivery failed (is webhook.enabled=true?)"
DELIVERY_ID=$(printf '%s' "${TEST}" | jq -r '.delivery_id')
[[ "${DELIVERY_ID}" =~ ^[0-9]+$ ]] || fail "invalid delivery_id: ${DELIVERY_ID}"
log "test delivery ${DELIVERY_ID} created"

STATUS=""
for _ in $(seq 1 30); do
  ROWS=$(curl -sf "${CUBE_OPS_ADDR}/api/v1/webhooks/${SUB_ID}/deliveries?event_id_prefix=test:" "${AUTH[@]}")
  STATUS=$(printf '%s' "${ROWS}" | jq -r --argjson id "${DELIVERY_ID}" \
    '.[] | select(.id == $id) | .status' | head -1)
  [[ "${STATUS}" == "succeeded" ]] && break
  sleep 1
done
[[ "${STATUS}" == "succeeded" ]] || fail "delivery ${DELIVERY_ID} not succeeded (status=${STATUS:-unknown})"
log "delivery succeeded"

# 6. Soft-delete the subscription (proves DELETE works and cleans the registry).
curl -sf -X DELETE "${CUBE_OPS_ADDR}/api/v1/webhooks/${SUB_ID}" "${AUTH[@]}" >/dev/null \
  || fail "subscription delete failed"
log "subscription ${SUB_ID} soft-deleted"

echo "[smoke] PASS: create → test delivery → HMAC POST → succeeded → delete"
