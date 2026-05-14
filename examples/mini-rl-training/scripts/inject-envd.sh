#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
ENVD_INJECT_DIR="$PROJECT_DIR/envd-inject"
ENVD_BIN="$ENVD_INJECT_DIR/envd"
# For mainland China (recommended) use the CN endpoint;
# for international access use the INT endpoint.
ENVD_SOURCE_IMAGE="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
# ENVD_SOURCE_IMAGE="cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"

usage() {
    echo "Usage: $0 <base-image> [output-tag]"
    echo ""
    echo "Inject envd into a Docker image to make it cube-sandbox compatible."
    echo ""
    echo "Arguments:"
    echo "  base-image   Source image (e.g. swebench/sweb.eval.x86_64.django_1776_django-13447:latest)"
    echo "  output-tag   Output image tag (default: <base-image>-envd:latest)"
    echo ""
    echo "Examples:"
    echo "  $0 swebench/sweb.eval.x86_64.django_1776_django-13447:latest"
    echo "  $0 ubuntu:22.04 my-ubuntu-envd:latest"
    exit 1
}

if [[ $# -lt 1 ]]; then
    usage
fi

BASE_IMAGE="$1"
OUTPUT_TAG="${2:-${BASE_IMAGE%:*}-envd:latest}"

echo "=== cube-sandbox: envd Injection ==="
echo ""

if [[ ! -f "$ENVD_BIN" ]]; then
    echo "envd binary not found. Extracting from $ENVD_SOURCE_IMAGE..."
    docker run --rm "$ENVD_SOURCE_IMAGE" cat /usr/bin/envd > "$ENVD_BIN"
    chmod +x "$ENVD_BIN"
    echo "envd extracted to $ENVD_BIN"
else
    echo "envd binary found at $ENVD_BIN"
fi

echo ""
echo "Base image:   $BASE_IMAGE"
echo "Output tag:   $OUTPUT_TAG"
echo ""

echo "--- Building injected image ---"
docker build \
    --build-arg BASE_IMAGE="$BASE_IMAGE" \
    -t "$OUTPUT_TAG" \
    -f "$ENVD_INJECT_DIR/Dockerfile" \
    "$ENVD_INJECT_DIR"

echo ""
echo "=== Done ==="
echo "Image built: $OUTPUT_TAG"
echo ""
echo "Next steps:"
echo "  1. Register this image as a cube-sandbox template"
echo "  2. Set the template_id in your .env file as CUBE_TEMPLATE_ID"
echo ""
echo "To test locally:"
echo "  docker run -d -p 49983:49983 $OUTPUT_TAG"
