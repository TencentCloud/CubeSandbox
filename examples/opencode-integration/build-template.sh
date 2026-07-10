#!/usr/bin/env bash
#
# Build and register the OpenCode CubeSandbox template.
#
# Usage:
#   ./build-template.sh
#
# Environment variables (all optional):
#   REGISTRY   - container registry prefix  (default: ghcr.io/tencentcloud)
#   IMAGE_NAME - image repository / name    (default: opencode-cube)
#   IMAGE_TAG  - image tag                  (default: latest)
#   WRITABLE_LAYER_SIZE - rootfs writable layer size (default: 4G)
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configurable defaults
# ---------------------------------------------------------------------------
REGISTRY="${REGISTRY:-ghcr.io/tencentcloud}"
IMAGE_NAME="${IMAGE_NAME:-opencode-cube}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
WRITABLE_LAYER_SIZE="${WRITABLE_LAYER_SIZE:-4G}"

FULL_IMAGE="${REGISTRY}/${IMAGE_NAME}:${IMAGE_TAG}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# Step 1 — Build the Docker image
# ---------------------------------------------------------------------------
echo "==> Building image: ${FULL_IMAGE}"
docker build \
    -t "${FULL_IMAGE}" \
    -f "${SCRIPT_DIR}/Dockerfile" \
    "${SCRIPT_DIR}"

# ---------------------------------------------------------------------------
# Step 2 — Push the image
# ---------------------------------------------------------------------------
echo "==> Pushing image: ${FULL_IMAGE}"
docker push "${FULL_IMAGE}"

# ---------------------------------------------------------------------------
# Step 3 — Register as a Cube template
# ---------------------------------------------------------------------------
echo "==> Creating template from image"
CREATE_OUTPUT="$(cubemastercli tpl create-from-image \
    --image "${FULL_IMAGE}" \
    --writable-layer-size "${WRITABLE_LAYER_SIZE}" \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health)"

echo "${CREATE_OUTPUT}"

JOB_ID="$(echo "${CREATE_OUTPUT}" | grep -E '^job_id:' | awk '{print $2}')"
TEMPLATE_ID="$(echo "${CREATE_OUTPUT}" | grep -E '^template_id:' | awk '{print $2}')"

if [[ -z "${JOB_ID}" ]]; then
    echo "ERROR: failed to parse job_id from create-from-image output" >&2
    exit 1
fi
if [[ -z "${TEMPLATE_ID}" ]]; then
    echo "ERROR: failed to parse template_id from create-from-image output" >&2
    exit 1
fi

echo "==> job_id=${JOB_ID}  template_id=${TEMPLATE_ID}"

# ---------------------------------------------------------------------------
# Step 4 — Watch the build job until it reaches a terminal state
# ---------------------------------------------------------------------------
echo "==> Watching build job (this may take a while)..."
WATCH_OUTPUT="$(cubemastercli tpl watch --job-id "${JOB_ID}")"
echo "${WATCH_OUTPUT}"

# ---------------------------------------------------------------------------
# Step 5 — Verify the template reached READY
# ---------------------------------------------------------------------------
FINAL_STATUS="$(echo "${WATCH_OUTPUT}" | grep -E '^status:' | tail -1 | awk '{print $2}')"

if [[ "${FINAL_STATUS}" != "READY" ]]; then
    echo "ERROR: template build did not reach READY (status=${FINAL_STATUS:-UNKNOWN})" >&2
    exit 1
fi

echo ""
echo "========================================="
echo " Template READY"
echo " template_id: ${TEMPLATE_ID}"
echo "========================================="
