#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
OUTPUT_DIR=${CUBEVZ_OUTPUT_DIR:-$ROOT_DIR/_output}
BIN_DIR=$OUTPUT_DIR/bin
GUEST_DIR=$OUTPUT_DIR/cube-vz/benchmark-guest
WORK_DIR=${CUBEVZ_LIFECYCLE_WORK_DIR:-$ROOT_DIR/.workdir/cube-vz}
TEMPLATE_DIR=$WORK_DIR/lifecycle-template
SANDBOXES_DIR=$WORK_DIR/sandboxes
PORT=${CUBEVZ_LIFECYCLE_PORT:-33000}
TEMPLATE_ID=${CUBEVZ_LIFECYCLE_TEMPLATE_ID:-cube-vz}
WARMUP=${CUBEVZ_LIFECYCLE_WARMUP:-3}
C1_TOTAL=${CUBEVZ_LIFECYCLE_C1_TOTAL:-20}
C10_TOTAL=${CUBEVZ_LIFECYCLE_C10_TOTAL:-200}
RESULTS_DIR=$OUTPUT_DIR/cube-vz/lifecycle-results/$(date -u +%Y%m%dT%H%M%SZ)
API_PID=

cleanup() {
  if [ -n "$API_PID" ]; then
    kill "$API_PID" 2>/dev/null || true
    wait "$API_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

if [ "${CUBEVZ_LIFECYCLE_SKIP_BUILD:-0}" != 1 ]; then
  make -C "$ROOT_DIR" cube-vz cube-vz-benchmark-guest
fi

for file in "$BIN_DIR/cube-vz" "$BIN_DIR/cube-vz-api" \
  "$GUEST_DIR/kernel" "$GUEST_DIR/initrd" "$GUEST_DIR/rootfs.raw"; do
  test -f "$file" || { echo "ERROR: missing artifact: $file" >&2; exit 1; }
done

rm -rf "$TEMPLATE_DIR" "$SANDBOXES_DIR"
mkdir -p "$SANDBOXES_DIR" "$RESULTS_DIR"

"$BIN_DIR/cube-vz" create \
  --vm-dir "$TEMPLATE_DIR" \
  --kernel "$GUEST_DIR/kernel" \
  --initrd "$GUEST_DIR/initrd" \
  --disk "$GUEST_DIR/rootfs.raw" \
  --cpus 2 \
  --memory-mib 2048 \
  --cmdline "console=hvc0 root=/dev/vda rw rootfstype=ext4 modules=ext4,virtio_blk,virtio_pci init=/usr/local/sbin/cube-vz-lifecycle-init"
"$BIN_DIR/cube-vz" prepare-template --vm-dir "$TEMPLATE_DIR" --timeout-seconds 30

docker run --rm \
  -e CGO_ENABLED=0 \
  -e GOOS=darwin \
  -e GOARCH=arm64 \
  -v "$ROOT_DIR:/workspace" \
  -w /workspace/examples/cube-bench \
  golang:1.25-alpine \
  /usr/local/go/bin/go build -trimpath -o /workspace/_output/bin/cube-bench-macos .

"$BIN_DIR/cube-vz-api" \
  --template-dir "$TEMPLATE_DIR" \
  --sandboxes-dir "$SANDBOXES_DIR" \
  --template-id "$TEMPLATE_ID" \
  --port "$PORT" >"$RESULTS_DIR/api.log" 2>&1 &
API_PID=$!

attempt=0
until [ "$(curl -fsS "http://127.0.0.1:$PORT/health" 2>/dev/null || true)" = '{"status":"ok"}' ]; do
  if ! kill -0 "$API_PID" 2>/dev/null; then
    echo "ERROR: cube-vz-api exited during startup" >&2
    cat "$RESULTS_DIR/api.log" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 100 ]; then
    echo "ERROR: cube-vz-api did not become ready" >&2
    exit 1
  fi
  sleep 0.05
done

export E2B_API_URL="http://127.0.0.1:$PORT"
export E2B_API_KEY=local
export CUBE_TEMPLATE_ID=$TEMPLATE_ID

"$BIN_DIR/cube-bench-macos" --no-tui \
  -c 1 -n "$C1_TOTAL" -w "$WARMUP" -m create-delete \
  -o "$RESULTS_DIR/c1.json"
"$BIN_DIR/cube-bench-macos" --no-tui \
  -c 10 -n "$C10_TOTAL" -w "$WARMUP" -m create-delete \
  -o "$RESULTS_DIR/c10.json"

echo "CubeVZ lifecycle benchmark results: $RESULTS_DIR"
