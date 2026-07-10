#!/usr/bin/env bash
set -euo pipefail

# Build a Gemini CLI image and register it as a CubeSandbox template.
# Example:
#   IMAGE=registry.example.com/cube/gemini-cli:2026-07-10 ./build-template.sh

: "${IMAGE:?Set IMAGE to a registry image name, for example registry.example.com/cube/gemini-cli:tag}"
: "${CUBE_TEMPLATE_NAME:=gemini-cli}"
: "${GEMINI_CLI_VERSION:=latest}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

docker build \
  --build-arg "GEMINI_CLI_VERSION=${GEMINI_CLI_VERSION}" \
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
