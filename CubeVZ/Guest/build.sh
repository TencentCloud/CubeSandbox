#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
OUTPUT_DIR="${REPO_ROOT}/_output/cube-vz/guest"
BASE_IMAGE="cube-sandbox-envd:cube-vz"

command -v docker >/dev/null 2>&1 || {
  echo "ERROR: docker is required to build the CubeVZ guest" >&2
  exit 1
}
docker info >/dev/null 2>&1 || {
  echo "ERROR: Docker Desktop is not running" >&2
  exit 1
}

docker buildx build \
  --platform linux/arm64 \
  --file "${REPO_ROOT}/docker/Dockerfile.cube-base" \
  --tag "${BASE_IMAGE}" \
  --load \
  "${REPO_ROOT}/docker"

rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"
docker buildx build \
  --platform linux/arm64 \
  --file "${SCRIPT_DIR}/Dockerfile" \
  --build-arg "BASE_IMAGE=${BASE_IMAGE}" \
  --output "type=local,dest=${OUTPUT_DIR}" \
  "${REPO_ROOT}"

for artifact in kernel initrd rootfs.raw SHA256SUMS build-info.txt; do
  test -s "${OUTPUT_DIR}/${artifact}" || {
    echo "ERROR: CubeVZ guest artifact is missing: ${OUTPUT_DIR}/${artifact}" >&2
    exit 1
  }
done

echo "CubeVZ guest ready: ${OUTPUT_DIR}"
ls -lh "${OUTPUT_DIR}"
