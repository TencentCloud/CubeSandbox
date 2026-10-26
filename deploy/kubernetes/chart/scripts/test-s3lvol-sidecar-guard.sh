#!/bin/sh
# Guard: cube-s3lvol is off by default; enabling it injects the sidecar,
# shared RPC socket, s3.cfg Secret, and NODE_NAME from spec.nodeName.
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

extract_big_pod() {
  input="$1"
  output="$2"
  python3 - "$input" "$output" <<'PY'
import pathlib
import re
import sys

documents = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
matches = [
    doc for doc in documents
    if "\nkind: DaemonSet\n" in f"\n{doc}\n"
    and "\n    app.kubernetes.io/component: cube-node\n" in f"\n{doc}\n"
    and re.search(r"(?m)^apiVersion:\s*apps/v1\s*$", doc)
]
if len(matches) != 1:
    raise SystemExit(f"expected one cube-node DaemonSet, found {len(matches)}")
pathlib.Path(sys.argv[2]).write_text(matches[0].strip() + "\n")
PY
}

helm template s3lvol-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
extract_big_pod "$TMP_DIR/default.yaml" "$TMP_DIR/default-node.yaml"
if grep -q 'name: cube-s3lvol' "$TMP_DIR/default-node.yaml"; then
  echo "default render must not include the cube-s3lvol container" >&2
  exit 1
fi
if grep -q 'name: s3lvol-rpc' "$TMP_DIR/default-node.yaml"; then
  echo "default render must not include the s3lvol-rpc volume" >&2
  exit 1
fi
if grep -q 'CUBE_S3LVOL_SOCKET' "$TMP_DIR/default-node.yaml"; then
  echo "default render must not set CUBE_S3LVOL_SOCKET on cubelet" >&2
  exit 1
fi
if grep -q 'app.kubernetes.io/component: cube-s3lvol' "$TMP_DIR/default.yaml"; then
  echo "default render must not create the cube-s3lvol Secret" >&2
  exit 1
fi
echo "ok: default render has no cube-s3lvol sidecar"

helm template s3lvol-on "$CHART_DIR" $COMMON_SETS \
  --set cubeS3lvol.enabled=true \
  > "$TMP_DIR/on.yaml"
extract_big_pod "$TMP_DIR/on.yaml" "$TMP_DIR/on-node.yaml"

grep -q 'name: cube-s3lvol' "$TMP_DIR/on-node.yaml" \
  || { echo "cubeS3lvol.enabled=true must add container cube-s3lvol" >&2; exit 1; }
grep -q 'name: s3lvol-rpc' "$TMP_DIR/on-node.yaml" \
  || { echo "enabled sidecar must mount s3lvol-rpc emptyDir" >&2; exit 1; }
grep -q 'name: s3lvol-cfg' "$TMP_DIR/on-node.yaml" \
  || { echo "enabled sidecar must mount s3lvol-cfg Secret" >&2; exit 1; }
grep -q 'CUBE_S3LVOL_SOCKET' "$TMP_DIR/on-node.yaml" \
  || { echo "enabled sidecar must set CUBE_S3LVOL_SOCKET on cubelet" >&2; exit 1; }
grep -q '/var/run/s3lvol/s3lvol.sock' "$TMP_DIR/on-node.yaml" \
  || { echo "enabled sidecar must use /var/run/s3lvol/s3lvol.sock" >&2; exit 1; }
grep -q 'app.kubernetes.io/component: cube-s3lvol' "$TMP_DIR/on.yaml" \
  || { echo "enabled sidecar must create the cube-s3lvol Secret" >&2; exit 1; }
grep -q 'buckets=\["cube-s3lvol"\]' "$TMP_DIR/on.yaml" \
  || { echo "s3.cfg must use the cube-s3lvol bucket, not cube-volumes" >&2; exit 1; }

python3 - "$TMP_DIR/on-node.yaml" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
# NODE_NAME in the cube-s3lvol container must be spec.nodeName (not the Pod name).
block = re.search(r"- name: cube-s3lvol\n(.*?)(?:\n      volumes:|\Z)", text, re.S)
if not block:
    raise SystemExit("cube-s3lvol container block not found")
body = block.group(1)
if "name: NODE_NAME" not in body:
    raise SystemExit("cube-s3lvol must set NODE_NAME")
if "fieldPath: spec.nodeName" not in body:
    raise SystemExit("cube-s3lvol NODE_NAME must use spec.nodeName")
if "RCOW_LVS_NAME" in body:
    raise SystemExit("default RCOW_LVS_NAME must be derived at runtime by rcow_common, not rendered")
if "livenessProbe:" in body:
    raise SystemExit("cube-s3lvol must not have a livenessProbe")
if "timeoutSeconds: 8" not in body:
    raise SystemExit("s3lvol probes must allow the 5s RPC timeout (timeoutSeconds >= 8)")
if "terminationGracePeriodSeconds: 180" not in text:
    raise SystemExit("enabling cube-s3lvol must raise terminationGracePeriodSeconds to at least 180")
print("ok: NODE_NAME is spec.nodeName; LVS name is derived at runtime; no liveness")
PY

echo "ok: cubeS3lvol.enabled=true injects sidecar + socket + s3.cfg"

expect_fail s3lvol-no-s3 "$TMP_DIR/no-s3.err" "S3" \
  --set cubeS3lvol.enabled=true \
  --set minio.enabled=false

expect_fail s3lvol-shared-bucket "$TMP_DIR/shared-bucket.err" "cube-volumes" \
  --set cubeS3lvol.enabled=true \
  --set-string cubeS3lvol.s3.bucket=cube-volumes

expect_fail s3lvol-same-vol-bucket "$TMP_DIR/same-vol-bucket.err" "volumeS3.bucket" \
  --set cubeS3lvol.enabled=true \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://s3.example.com \
  --set-string volumeS3.accessKeyId=ak \
  --set-string volumeS3.secretAccessKey=sk \
  --set-string volumeS3.bucket=shared-bucket \
  --set-string cubeS3lvol.s3.bucket=shared-bucket

helm template s3lvol-external "$CHART_DIR" $COMMON_SETS \
  --set cubeS3lvol.enabled=true \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://s3.example.com \
  --set-string volumeS3.accessKeyId=ak \
  --set-string volumeS3.secretAccessKey=sk \
  --set-string volumeS3.bucket=cube-volumes \
  > "$TMP_DIR/external.yaml"
grep -q 'buckets=\["cube-s3lvol"\]' "$TMP_DIR/external.yaml" \
  || { echo "external volumeS3 must still use cube-s3lvol bucket" >&2; exit 1; }
grep -q 'endpoint="s3.example.com"' "$TMP_DIR/external.yaml" \
  || { echo "s3.cfg endpoint must strip https://" >&2; exit 1; }
if grep -q 'no_tls="true"' "$TMP_DIR/external.yaml"; then
  echo "https endpoint must not set no_tls" >&2
  exit 1
fi
echo "ok: external S3 fill keeps cube-s3lvol bucket and strips scheme"

helm template s3lvol-lvs "$CHART_DIR" $COMMON_SETS \
  --set cubeS3lvol.enabled=true \
  --set-string cubeS3lvol.lvsName=rcow-explicit \
  > "$TMP_DIR/lvs.yaml"
grep -q 'name: RCOW_LVS_NAME' "$TMP_DIR/lvs.yaml" \
  || { echo "cubeS3lvol.lvsName must render RCOW_LVS_NAME" >&2; exit 1; }
grep -q 'rcow-explicit' "$TMP_DIR/lvs.yaml" \
  || { echo "explicit lvsName must appear in the sidecar env" >&2; exit 1; }
echo "ok: cubeS3lvol.lvsName override is rendered"

echo "cube-s3lvol sidecar guard passed"
