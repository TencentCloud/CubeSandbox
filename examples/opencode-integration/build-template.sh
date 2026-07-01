#!/usr/bin/env bash
set -euo pipefail

IMAGE_REGISTRY="${IMAGE_REGISTRY:-cube-sandbox-int.tencentcloudcr.com/cube-sandbox}"
IMAGE_NAME="${IMAGE_NAME:-opencode-agent}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE="${IMAGE_REGISTRY}/${IMAGE_NAME}:${IMAGE_TAG}"
WRITABLE_LAYER_SIZE="${WRITABLE_LAYER_SIZE:-4G}"

cd "$(dirname "$0")"

docker build -t "${IMAGE}" -f template/Dockerfile template

if [[ "${PUSH_IMAGE:-0}" == "1" ]]; then
  docker push "${IMAGE}"
fi

cat <<EOF
Image built: ${IMAGE}

Create a Cube Sandbox template with:

cubemastercli tpl create-from-image \\
  --image ${IMAGE} \\
  --writable-layer-size ${WRITABLE_LAYER_SIZE} \\
  --expose-port 49999 \\
  --expose-port 49983 \\
  --probe 49999

Set CUBE_TEMPLATE_ID to the returned template_id.
EOF
