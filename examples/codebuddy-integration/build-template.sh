#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -z "${IMAGE_NAME:-}" ]]; then
  cat >&2 <<'EOF'
IMAGE_NAME is required.

Example:
  IMAGE_NAME=registry.example.com/cube/codebuddy:latest bash build-template.sh

Optional:
  DOCKER_PLATFORM=linux/amd64 Build for the target Cube node architecture.
  PUSH_IMAGE=1      Push the built image.
  CREATE_TEMPLATE=1 Create a Cube template after the image is available.
  WATCH_JOB=1       Watch the template build job when CREATE_TEMPLATE=1.
EOF
  exit 1
fi

docker build --platform "${DOCKER_PLATFORM:-linux/amd64}" -t "${IMAGE_NAME}" "${SCRIPT_DIR}/template"

if [[ "${PUSH_IMAGE:-0}" == "1" ]]; then
  docker push "${IMAGE_NAME}"
fi

if [[ "${CREATE_TEMPLATE:-0}" == "1" ]]; then
  create_output="$(
    cubemastercli tpl create-from-image \
      --image "${IMAGE_NAME}" \
      --writable-layer-size "${WRITABLE_LAYER_SIZE:-2G}" \
      --expose-port 49983 \
      --probe 49983 \
      --probe-path /health
  )"
  printf '%s\n' "${create_output}"

  if [[ "${WATCH_JOB:-0}" == "1" ]]; then
    job_id="$(printf '%s\n' "${create_output}" | sed -n 's/.*job_id[=: ]*\([^ ]*\).*/\1/p' | tail -n 1)"
    if [[ -n "${job_id}" ]]; then
      cubemastercli tpl watch --job-id "${job_id}"
    else
      echo "Could not infer job_id from create-from-image output; run cubemastercli tpl status manually." >&2
    fi
  fi
fi
