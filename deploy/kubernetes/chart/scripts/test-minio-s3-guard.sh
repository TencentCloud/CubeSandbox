#!/bin/sh
# Guard: minio.* installs MinIO; volumeS3.* is the plugin source of truth.
# They are mutually exclusive when the operator supplies an external S3 backend.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

expect_fail() {
  name="$1"
  err_file="$2"
  needle="$3"
  shift 3
  if helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >/dev/null 2>"$err_file"; then
    echo "expected fail: $name" >&2
    exit 1
  fi
  grep -qi "$needle" "$err_file" || {
    echo "unexpected error for $name (wanted /$needle/):" >&2
    cat "$err_file" >&2
    exit 1
  }
}

# Default values deploy MinIO and fill volume-s3.conf from that MinIO
# (S3 plugin source of truth; minio.* only installs the server).
helm template minio-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
grep -q 'app.kubernetes.io/component: minio' "$TMP_DIR/default.yaml" || {
  echo "default render must include chart-managed MinIO" >&2
  exit 1
}
grep -q 'RELEASE.2025-09-07T16-13-09Z' "$TMP_DIR/default.yaml" || {
  echo "default MinIO image must use RELEASE.2025-09-07T16-13-09Z" >&2
  exit 1
}
grep -q 'app.kubernetes.io/component: volume-s3' "$TMP_DIR/default.yaml" || {
  echo "default render must include volume-s3 Secret" >&2
  exit 1
}
grep -q "ACCESS_KEY_ID='cubeminio'" "$TMP_DIR/default.yaml" || {
  echo "default volume-s3.conf must use MinIO root user as S3 access key" >&2
  exit 1
}
grep -q "BUCKET='cube-volumes'" "$TMP_DIR/default.yaml" || {
  echo "default volume-s3.conf must use MinIO bucket as S3 bucket" >&2
  exit 1
}
grep -q "ENDPOINT='http://" "$TMP_DIR/default.yaml" || {
  echo "default volume-s3.conf must use the in-cluster MinIO HTTP endpoint" >&2
  exit 1
}
grep -q -- '-minio.' "$TMP_DIR/default.yaml" || {
  echo "default volume-s3.conf endpoint must point at the chart MinIO Service" >&2
  exit 1
}
grep -q "S3FS_EXTRA_OPTS='-ouse_path_request_style'" "$TMP_DIR/default.yaml" || {
  echo "chart MinIO volume-s3.conf must force path-style s3fs options" >&2
  exit 1
}

# External S3 requires minio.enabled=false; the two config families do not overlap.
helm template minio-external "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://s3.example.com \
  --set-string volumeS3.accessKeyId=ak \
  --set-string volumeS3.secretAccessKey=sk \
  --set-string volumeS3.bucket=my-volumes \
  --set-string volumeS3.extraOpts="-ouse_path_request_style -oallow_other" \
  > "$TMP_DIR/external.yaml"
if grep -q 'app.kubernetes.io/component: minio' "$TMP_DIR/external.yaml"; then
  echo "minio.enabled=false + volumeS3.endpoint must not deploy chart MinIO" >&2
  exit 1
fi
grep -q 'app.kubernetes.io/component: volume-s3' "$TMP_DIR/external.yaml" || {
  echo "volumeS3.endpoint must still create volume-s3 Secret" >&2
  exit 1
}
grep -q "ENDPOINT='https://s3.example.com'" "$TMP_DIR/external.yaml" || {
  echo "volume-s3.conf must use the operator endpoint" >&2
  exit 1
}
grep -q "S3FS_EXTRA_OPTS='-ouse_path_request_style -oallow_other'" "$TMP_DIR/external.yaml" || {
  echo "volume-s3.conf must quote whitespace-separated extraOpts" >&2
  exit 1
}

# existingSecret also requires minio.enabled=false and does not create the chart Secret.
helm template minio-existing "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  --set-string volumeS3.existingSecret=my-s3-secret \
  > "$TMP_DIR/existing.yaml"
if grep -q 'app.kubernetes.io/component: minio' "$TMP_DIR/existing.yaml"; then
  echo "minio.enabled=false + volumeS3.existingSecret must not deploy chart MinIO" >&2
  exit 1
fi
if grep -q 'app.kubernetes.io/component: volume-s3' "$TMP_DIR/existing.yaml"; then
  echo "volumeS3.existingSecret must not create a chart volume-s3 Secret" >&2
  exit 1
fi
grep -q 'secretName: my-s3-secret' "$TMP_DIR/existing.yaml" || {
  echo "master/cubelet must mount volumeS3.existingSecret" >&2
  exit 1
}

# minio.enabled=false without an external S3 backend deploys neither.
helm template minio-off "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  > "$TMP_DIR/off.yaml"
if grep -q 'app.kubernetes.io/component: minio' "$TMP_DIR/off.yaml"; then
  echo "minio.enabled=false must skip chart-managed MinIO" >&2
  exit 1
fi
if grep -q 'app.kubernetes.io/component: volume-s3' "$TMP_DIR/off.yaml"; then
  echo "minio.enabled=false without volumeS3.endpoint must not create volume-s3 Secret" >&2
  exit 1
fi

# Combining both config families must fail.
expect_fail minio-and-s3 "$TMP_DIR/both.err" \
  'mutually exclusive' \
  --set-string volumeS3.endpoint=https://s3.example.com \
  --set-string volumeS3.accessKeyId=ak \
  --set-string volumeS3.secretAccessKey=sk \
  --set-string volumeS3.bucket=my-volumes

# Incomplete external S3 config must fail (MinIO off so the exclusive check
# does not hide the missing-keys error).
expect_fail minio-s3-incomplete "$TMP_DIR/incomplete.err" \
  'volumeS3.endpoint requires' \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://s3.example.com

echo "MinIO / volumeS3 skip guard passed"
