#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
BIN_DIR="${REPO_ROOT}/_output/bin"
GUEST_DIR="${REPO_ROOT}/_output/cube-vz/guest"
WORK_DIR="${REPO_ROOT}/.workdir/cube-vz/smoke"
TEMPLATE_DIR="${WORK_DIR}/template"
SANDBOXES_DIR="${WORK_DIR}/sandboxes"
PORT=34000
API_PID=
SANDBOX_ID=

cleanup() {
  if [ -n "${SANDBOX_ID}" ]; then
    curl -fsS -X DELETE "http://127.0.0.1:${PORT}/sandboxes/${SANDBOX_ID}" >/dev/null || true
  fi
  if [ -n "${API_PID}" ]; then
    kill "${API_PID}" 2>/dev/null || true
    wait "${API_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

for artifact in kernel rootfs.raw; do
  test -s "${GUEST_DIR}/${artifact}" || {
    echo "ERROR: missing ${GUEST_DIR}/${artifact}; run make cube-vz-guest" >&2
    exit 1
  }
done

docker run --rm \
  -e CGO_ENABLED=0 \
  -e GOOS=darwin \
  -e GOARCH=arm64 \
  -v "${REPO_ROOT}:/workspace" \
  -w /workspace/CubeVZ/Guest/SmokeClient \
  golang:1.25-alpine \
  go build -trimpath -o /workspace/_output/bin/cube-vz-smoke-client .

rm -rf "${WORK_DIR}"
mkdir -p "${SANDBOXES_DIR}"
"${BIN_DIR}/cube-vz" create \
  --vm-dir "${TEMPLATE_DIR}" \
  --kernel "${GUEST_DIR}/kernel" \
  --disk "${GUEST_DIR}/rootfs.raw" \
  --cpus 2 \
  --memory-mib 2048 \
  --cmdline "console=hvc0 quiet loglevel=0 root=/dev/vda rw rootfstype=ext4 init=/usr/local/sbin/cube-vz-init"
"${BIN_DIR}/cube-vz-api" \
  --template-dir "${TEMPLATE_DIR}" \
  --sandboxes-dir "${SANDBOXES_DIR}" \
  --port "${PORT}" >"${WORK_DIR}/api.log" 2>&1 &
API_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then break; fi
  sleep 0.05
done

response="$(curl -fsS \
  -H 'Content-Type: application/json' \
  -d '{"templateID":"cube-vz"}' \
  "http://127.0.0.1:${PORT}/sandboxes")"
SANDBOX_ID="$(printf '%s' "${response}" | sed -n 's/.*"sandboxID":"\([^"]*\)".*/\1/p')"
test -n "${SANDBOX_ID}" || {
  echo "ERROR: create response has no sandboxID: ${response}" >&2
  exit 1
}

status="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Host: 49983-${SANDBOX_ID}.cube.local" \
  "http://127.0.0.1:$((PORT + 1))/health")"
test "${status}" = 204 || {
  echo "ERROR: envd health returned HTTP ${status}" >&2
  cat "${WORK_DIR}/api.log" >&2
  exit 1
}

echo "PASS CubeVZ lifecycle -> transparent vsock relay -> existing envd"

CUBE_API_URL="http://127.0.0.1:${PORT}" \
CUBE_TEMPLATE_ID=cube-vz \
CUBE_PROXY_NODE_IP=127.0.0.1 \
CUBE_PROXY_PORT_HTTP="$((PORT + 1))" \
CUBE_PROXY_SCHEME=http \
CUBE_SANDBOX_DOMAIN=cube.local \
  "${BIN_DIR}/cube-vz-smoke-client"
