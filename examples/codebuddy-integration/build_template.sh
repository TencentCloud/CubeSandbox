#!/usr/bin/env bash
set -euo pipefail

# Build the Docker image and push to a registry, then register as a CubeSandbox template.
#
# Usage:
#   ./build_template.sh --registry <registry-url> [--image-name codebuddy-sandbox] [--tag latest]
#
# Environment variables:
#   CUBE_API_URL   CubeAPI address (default: http://127.0.0.1:3000)

REGISTRY=""
IMAGE_NAME="codebuddy-sandbox"
TAG="latest"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --registry) REGISTRY="$2"; shift 2 ;;
    --image-name) IMAGE_NAME="$2"; shift 2 ;;
    --tag) TAG="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

if [[ -z "$REGISTRY" ]]; then
  echo "Error: --registry is required"
  echo "Usage: $0 --registry <registry-url> [--image-name codebuddy-sandbox] [--tag latest]"
  exit 1
fi

FULL_IMAGE="${REGISTRY}/${IMAGE_NAME}:${TAG}"

echo "=== Building Docker image: ${FULL_IMAGE} ==="
docker build -t "${FULL_IMAGE}" template

echo "=== Pushing to registry ==="
docker push "${FULL_IMAGE}"

echo "=== Registering as CubeSandbox template ==="
cubemastercli tpl create-from-image \
  --image "${FULL_IMAGE}" \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --cpu 4000 --memory 4096 \
  --probe 49983

echo ""
echo "=== Done! ==="
echo "Set CUBE_TEMPLATE_ID to the template ID printed above."
