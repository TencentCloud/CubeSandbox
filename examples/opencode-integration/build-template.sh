#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:-opencode-cube:1.18.9}"
PLATFORM="${PLATFORM:-linux/amd64}"
PUSH="${PUSH:-0}"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

docker build \
  --platform "${PLATFORM}" \
  --tag "${IMAGE}" \
  "${script_dir}"

if [[ "${PUSH}" == "1" ]]; then
  docker push "${IMAGE}"
fi

printf 'Built %s for %s\n' "${IMAGE}" "${PLATFORM}"
if [[ "${PUSH}" != "1" ]]; then
  printf 'Set PUSH=1 and IMAGE=<registry>/opencode-cube:1.18.9 to push it.\n'
fi
