#!/usr/bin/env bash
set -euo pipefail

# Build a Claude Code image and register it as a CubeSandbox template.
# Example:
#   IMAGE=registry.example.com/cube/claude-code:2026-07-29 ./build-template.sh

: "${IMAGE:?Set IMAGE to a registry image name, for example registry.example.com/cube/claude-code:tag}"
: "${CUBE_TEMPLATE_NAME:=claude-code}"
: "${CLAUDE_CODE_VERSION:=latest}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

docker build \
  --build-arg "CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}" \
  -t "${IMAGE}" \
  "${SCRIPT_DIR}"
docker push "${IMAGE}"

cubemastercli tpl create-from-image \
  --image "${IMAGE}" \
  --name "${CUBE_TEMPLATE_NAME}" \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
