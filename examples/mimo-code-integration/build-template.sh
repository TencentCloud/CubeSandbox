#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

: "${MIMO_IMAGE:?Set MIMO_IMAGE to a registry image, for example registry.example.com/cube/mimo-code:0.1.7}"

MIMO_VERSION="${MIMO_VERSION:-0.1.7}"
CUBE_BASE_IMAGE="${CUBE_BASE_IMAGE:-ghcr.io/tencentcloud/cubesandbox-base:2026.16}"
CUBE_WRITABLE_LAYER_SIZE="${CUBE_WRITABLE_LAYER_SIZE:-4G}"

docker build \
  --platform linux/amd64 \
  --build-arg "CUBE_BASE_IMAGE=${CUBE_BASE_IMAGE}" \
  --build-arg "MIMO_VERSION=${MIMO_VERSION}" \
  --tag "${MIMO_IMAGE}" \
  "${SCRIPT_DIR}"

docker push "${MIMO_IMAGE}"

cubemastercli tpl create-from-image \
  --image "${MIMO_IMAGE}" \
  --writable-layer-size "${CUBE_WRITABLE_LAYER_SIZE}" \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

printf '%s\n' \
  "Template import submitted." \
  "Run 'cubemastercli tpl watch --job-id <job_id>' with the job ID above."
