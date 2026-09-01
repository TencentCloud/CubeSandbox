#!/bin/sh
# Guard: warehouse blobs are in S3. Empty endpoint without MinIO/volumeS3
# still renders (warehouse disabled). An endpoint without CubeOps credentials
# must fail. Default AK/SK come from secretKeyRef. replicas>1 renders
# RollingUpdate and no ops-warehouse PVC.
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

extract_component_doc() {
  component="$1"
  input="$2"
  output="$3"
  awk -v component="$component" '
    BEGIN { RS="\n---\n"; ORS="\n---\n" }
    index($0, "app.kubernetes.io/component: " component) { print; found=1 }
    END { exit found ? 0 : 1 }
  ' "$input" > "$output"
}

helm template ops-warehouse-no-s3 "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  > "$TMP_DIR/nos3.yaml"
extract_component_doc ops "$TMP_DIR/nos3.yaml" "$TMP_DIR/ops-nos3.yaml"
python3 - "$TMP_DIR/ops-nos3.yaml" <<'PY'
import pathlib, re, sys
ops = pathlib.Path(sys.argv[1]).read_text()
if not re.search(r"name: CUBE_OPS_S3_ENDPOINT\n\s+value: \"\"", ops):
    raise SystemExit("minio.enabled=false without S3 must render empty CUBE_OPS_S3_ENDPOINT")
print("ok: minio.enabled=false without S3 renders (warehouse disabled)")
PY

expect_fail ops-persistence-removed "$TMP_DIR/persist.err" \
  'cubeOps.persistence was removed' \
  --set cubeOps.persistence.enabled=true

expect_fail ops-existing-secret-no-endpoint "$TMP_DIR/exist-noep.err" \
  'cubeOps.s3.existingSecret requires cubeOps.s3.endpoint' \
  --set minio.enabled=false \
  --set-string cubeOps.s3.existingSecret=my-ops-s3

expect_fail ops-volume-secret-no-creds "$TMP_DIR/volsec.err" \
  'volumeS3.existingSecret' \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://cos.example \
  --set-string volumeS3.existingSecret=my-vol-s3

helm template cubeops-s3-default "$CHART_DIR" $COMMON_SETS \
  > "$TMP_DIR/default.yaml"
extract_component_doc ops "$TMP_DIR/default.yaml" "$TMP_DIR/ops-default.yaml"

helm template cubeops-s3-ha "$CHART_DIR" $COMMON_SETS \
  --set cubeOps.replicas=3 \
  > "$TMP_DIR/ha.yaml"
extract_component_doc ops "$TMP_DIR/ha.yaml" "$TMP_DIR/ops-ha.yaml"

python3 - "$TMP_DIR/ops-default.yaml" "$TMP_DIR/ops-ha.yaml" "$TMP_DIR/default.yaml" "$TMP_DIR/ha.yaml" <<'PY'
import pathlib
import re
import sys

default_ops, ha_ops, default_all, ha_all = (pathlib.Path(p).read_text() for p in sys.argv[1:])

def strategy_type(deploy):
    m = re.search(r"(?m)^  strategy:\n(?:    .*\n)*?    type:\s*(\S+)\s*$", deploy)
    if not m:
        m = re.search(r"(?m)^  strategy:\n    type:\s*(\S+)\s*$", deploy)
    if not m:
        raise SystemExit("missing strategy.type on cube-ops")
    return m.group(1)

def has_warehouse_pvc(manifest):
    for doc in manifest.split("\n---\n"):
        if "kind: PersistentVolumeClaim" not in doc:
            continue
        m = re.search(r"(?m)^  name:\s*(\S+)", doc)
        if m and "warehouse" in m.group(1).lower():
            return True
    return False

if "secretKeyRef" not in default_ops or "s3-access-key-id" not in default_ops:
    raise SystemExit("default cube-ops must mount S3 AK via secretKeyRef")
if "secretKeyRef" not in default_ops or "s3-secret-access-key" not in default_ops:
    raise SystemExit("default cube-ops must mount S3 SK via secretKeyRef")
if re.search(r"name: CUBE_OPS_S3_ACCESS_KEY_ID\n\s+value: ", default_ops):
    raise SystemExit("S3 access key must not be a literal env value")
if "CUBE_OPS_WAREHOUSE_S3_" in default_ops:
    raise SystemExit("CUBE_OPS_WAREHOUSE_S3_* must not be rendered")
if not re.search(r"name: CUBE_OPS_S3_BUCKET\n\s+value: \"cube-ops\"", default_ops):
    raise SystemExit("CUBE_OPS_S3_BUCKET must be cube-ops")
if "CUBE_OPS_WAREHOUSE_PREFIX" in default_ops:
    raise SystemExit("CUBE_OPS_WAREHOUSE_PREFIX must not be rendered")
if "CUBE_OPS_WAREHOUSE_DIR" in default_ops:
    raise SystemExit("CUBE_OPS_WAREHOUSE_DIR must not be rendered")
if "kind: PersistentVolumeClaim" in default_ops or has_warehouse_pvc(default_all):
    raise SystemExit("warehouse PVC must not be rendered")
if strategy_type(default_ops) != "RollingUpdate":
    raise SystemExit(f"default strategy must be RollingUpdate, got {strategy_type(default_ops)!r}")
print("ok: default secretKeyRef + no PVC + RollingUpdate")

if "replicas: 3" not in ha_ops:
    raise SystemExit("replicas=3 must render")
if strategy_type(ha_ops) != "RollingUpdate":
    raise SystemExit(f"ha strategy must be RollingUpdate, got {strategy_type(ha_ops)!r}")
if "maxUnavailable: 0" not in ha_ops:
    raise SystemExit("ha RollingUpdate must set maxUnavailable 0")
if "kind: PersistentVolumeClaim" in ha_ops or has_warehouse_pvc(ha_all):
    raise SystemExit("replicas=3 must not render ops-warehouse PVC")
print("ok: replicas=3 RollingUpdate without warehouse PVC")
PY

helm template cubeops-s3-volbucket "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://cos.example \
  --set-string volumeS3.accessKeyId=ak \
  --set-string volumeS3.secretAccessKey=sk \
  --set-string volumeS3.bucket=cube-volumes \
  > "$TMP_DIR/vol.yaml"
extract_component_doc ops "$TMP_DIR/vol.yaml" "$TMP_DIR/ops-vol.yaml"
python3 - "$TMP_DIR/ops-vol.yaml" <<'PY'
import pathlib, re, sys
ops = pathlib.Path(sys.argv[1]).read_text()
if not re.search(r"name: CUBE_OPS_S3_BUCKET\n\s+value: \"cube-ops\"", ops):
    raise SystemExit("volumeS3.bucket must not become CUBE_OPS_S3_BUCKET")
if re.search(r"name: CUBE_OPS_S3_BUCKET\n\s+value: \"cube-volumes\"", ops):
    raise SystemExit("CUBE_OPS_S3_BUCKET leaked cube-volumes")
print("ok: volumeS3.bucket does not override cube-ops")
PY

helm template cubeops-bool-false "$CHART_DIR" $COMMON_SETS \
  --set cubeOps.s3.pathStyle=false \
  --set cubeOps.s3.createBucket=false \
  > "$TMP_DIR/bool.yaml"
extract_component_doc ops "$TMP_DIR/bool.yaml" "$TMP_DIR/ops-bool.yaml"
python3 - "$TMP_DIR/ops-bool.yaml" <<'PY'
import pathlib, re, sys
ops = pathlib.Path(sys.argv[1]).read_text()
if not re.search(r"name: CUBE_OPS_S3_PATH_STYLE\n\s+value: \"false\"", ops):
    raise SystemExit("pathStyle=false must render CUBE_OPS_S3_PATH_STYLE=false")
if not re.search(r"name: CUBE_OPS_S3_CREATE_BUCKET\n\s+value: \"false\"", ops):
    raise SystemExit("createBucket=false must render CUBE_OPS_S3_CREATE_BUCKET=false")
print("ok: pathStyle/createBucket false are not swallowed")
PY

helm template cubeops-existing-secret "$CHART_DIR" $COMMON_SETS \
  --set minio.enabled=false \
  --set-string cubeOps.s3.endpoint=https://cos.example \
  --set-string cubeOps.s3.existingSecret=my-ops-s3 \
  > "$TMP_DIR/exist.yaml"
extract_component_doc ops "$TMP_DIR/exist.yaml" "$TMP_DIR/ops-exist.yaml"
python3 - "$TMP_DIR/ops-exist.yaml" <<'PY'
import pathlib, re, sys
ops = pathlib.Path(sys.argv[1]).read_text()
if not re.search(r"name: CUBE_OPS_S3_ACCESS_KEY_ID\n\s+valueFrom:\n\s+secretKeyRef:\n\s+name: \"my-ops-s3\"", ops):
    raise SystemExit("cubeOps.s3.existingSecret must be the secretKeyRef name")
if re.search(r"name: CUBE_OPS_S3_ACCESS_KEY_ID\n\s+valueFrom:\n\s+secretKeyRef:\n\s+name: \".*-cube-secret\"", ops):
    raise SystemExit("existingSecret must not fall back to the chart secret")
print("ok: existingSecret + endpoint points secretKeyRef at that Secret")
PY

echo "All cube-ops warehouse S3 guard tests passed"
