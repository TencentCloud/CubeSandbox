#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
# MinIO is opt-in via CUBE_SANDBOX_MINIO_ENABLED. When it is off the local
# container is never started (see up-support.sh), so there is nothing to
# health-check. Skip rather than block on a missing container and then
# trigger Restart=on-failure.
if [[ "${CUBE_SANDBOX_MINIO_ENABLED:-1}" != "1" ]]; then
  exit 0
fi
wait_for_container_health "${CUBE_SANDBOX_MINIO_CONTAINER:-cube-sandbox-minio}" 40 2 || die "minio container not ready"
# Confirm the S3 API is serving, not just that the container process exists.
minio_port="${CUBE_SANDBOX_MINIO_API_PORT:-9000}"
minio_bind="${CUBE_SANDBOX_MINIO_API_BIND:-${CUBE_SANDBOX_NODE_IP:-127.0.0.1}}"
wait_for_http "http://${minio_bind}:${minio_port}/minio/health/live" 40 2 \
  || die "minio API did not become ready on ${minio_bind}:${minio_port}"

# The bundled MinIO starts with no buckets. TemplateCenter and the S3 volume
# plugin both use this bucket, so ensure it exists as part of startup.
bucket_tool="${TOOLBOX_ROOT}/CubeS3lvol/scripts/s3_bucket.py"
ensure_file "${bucket_tool}"
AWS_ACCESS_KEY_ID="${CUBE_SANDBOX_MINIO_ROOT_USER:?}" \
AWS_SECRET_ACCESS_KEY="${CUBE_SANDBOX_MINIO_ROOT_PASSWORD:?}" \
  python3 "${bucket_tool}" ensure \
    -e "${minio_bind}:${minio_port}" \
    -b "${CUBE_SANDBOX_MINIO_BUCKET:-cube-volumes}" \
    -r "${CUBE_S3_REGION:-us-east-1}" --path-style --no-tls \
  || die "could not ensure bundled MinIO bucket ${CUBE_SANDBOX_MINIO_BUCKET:-cube-volumes}"
