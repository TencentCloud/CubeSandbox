#!/bin/sh
# Guard: the Go artifact and warehouse clients support an explicit S3
# addressing style while leaving legacy booleans and volume clients alone.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Pin MinIO credentials so volume Secret comparisons only track settings.
COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test --set-string minio.rootPassword=test-minio-password"

render() {
  name="$1"
  shift
  helm template s3-addressing "$CHART_DIR" $COMMON_SETS "$@" > "$TMP_DIR/$name.yaml"
}

expect_fail() {
  setting="$1"
  shift
  if helm template s3-addressing "$CHART_DIR" $COMMON_SETS \
    "$@" > /dev/null 2> "$TMP_DIR/invalid.err"; then
    echo "expected invalid $setting to fail" >&2
    exit 1
  fi
  grep -Fq "$setting must be empty, auto, path, or virtual" "$TMP_DIR/invalid.err" || {
    cat "$TMP_DIR/invalid.err" >&2
    exit 1
  }
}

render default
render legacy --set controlPlane.artifactStore.s3Backed=true
render legacy-false --set controlPlane.artifactStore.s3Backed=true \
  --set cubeOps.s3.pathStyle=false --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://storage.example.test \
  --set-string volumeS3.accessKeyId=test-ak --set-string volumeS3.secretAccessKey=test-sk \
  --set-string volumeS3.bucket=artifacts \
  --set volumeS3.pathStyle=false

for style in auto path virtual; do
  render "$style" --set controlPlane.artifactStore.s3Backed=true \
    --set-string "cubeOps.s3.addressingStyle=$style" \
    --set-string "controlPlane.artifactStore.s3AddressingStyle=$style"
done

# A complete operator-owned credential set must not swallow the independent
# chart addressing style. global.env and cubeOps.env still take precedence.
cat > "$TMP_DIR/manual-values.yaml" <<'YAML'
minio:
  enabled: false
controlPlane:
  artifactStore:
    s3Backed: true
    s3AddressingStyle: virtual
global:
  env:
    - name: CUBE_S3_ENDPOINT
      value: https://storage.example.test
    - name: CUBE_S3_BUCKET
      value: artifacts
    - name: CUBE_S3_ACCESS_KEY_ID
      value: test-ak
    - name: CUBE_S3_SECRET_ACCESS_KEY
      value: test-sk
YAML
render manual -f "$TMP_DIR/manual-values.yaml"
render override -f "$TMP_DIR/manual-values.yaml" \
  --set-string 'global.env[4].name=CUBE_S3_ADDRESSING_STYLE' \
  --set-string 'global.env[4].value=path' \
  --set-string cubeOps.s3.addressingStyle=virtual \
  --set-string 'cubeOps.env[0].name=CUBE_OPS_S3_ADDRESSING_STYLE' \
  --set-string 'cubeOps.env[0].value=path'
render disabled --set-string controlPlane.artifactStore.s3AddressingStyle=virtual

render volume-default --set cubeS3lvol.enabled=true
render volume-virtual --set cubeS3lvol.enabled=true \
  --set controlPlane.artifactStore.s3Backed=true \
  --set-string controlPlane.artifactStore.s3AddressingStyle=virtual \
  --set-string cubeOps.s3.addressingStyle=virtual

python3 - "$TMP_DIR" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])

def docs(name):
    return (root / (name + ".yaml")).read_text().split("\n---\n")

def component(name, label, kind="Deployment"):
    matches = [doc for doc in docs(name)
               if re.search(r"(?m)^kind: " + kind + r"\s*$", doc)
               and re.search(r"(?m)^    app.kubernetes.io/component: " + label + r"\s*$", doc)]
    if len(matches) != 1:
        raise SystemExit(f"{name}: expected one {kind} for {label}, got {len(matches)}")
    return matches[0]

def env(name, label, key):
    doc = component(name, label)
    return [value.strip().strip('"') for value in re.findall(
        r"name: " + re.escape(key) + r'\n\s+value: ([^\n]*)', doc)]

def want(name, label, key, expected):
    actual = env(name, label, key)
    if actual != expected:
        raise SystemExit(f"{name}/{label}: {key}={actual!r}, want {expected!r}")

for name in ("default", "legacy", "legacy-false"):
    want(name, "ops", "CUBE_OPS_S3_ADDRESSING_STYLE", [])
    for label in ("master", "templatecenter"):
        want(name, label, "CUBE_S3_ADDRESSING_STYLE", [])
want("default", "ops", "CUBE_OPS_S3_PATH_STYLE", ["true"])
want("legacy-false", "ops", "CUBE_OPS_S3_PATH_STYLE", ["false"])
for label in ("master", "templatecenter"):
    want("legacy", label, "CUBE_S3_USE_PATH_STYLE", ["true"])
    want("legacy-false", label, "CUBE_S3_USE_PATH_STYLE", ["false"])
print("ok: empty style preserves legacy booleans and injects no new env")

for style in ("auto", "path", "virtual"):
    want(style, "ops", "CUBE_OPS_S3_ADDRESSING_STYLE", [style])
    want(style, "ops", "CUBE_OPS_S3_PATH_STYLE", ["true"])
    for label in ("master", "templatecenter"):
        want(style, label, "CUBE_S3_ADDRESSING_STYLE", [style])
        want(style, label, "CUBE_S3_USE_PATH_STYLE", ["true"])
print("ok: all explicit styles reach warehouse, Master and TC")

for label in ("master", "templatecenter"):
    want("manual", label, "CUBE_S3_ADDRESSING_STYLE", ["virtual"])
    want("manual", label, "CUBE_S3_ENDPOINT", ["https://storage.example.test"])
    want("manual", label, "CUBE_S3_USE_PATH_STYLE", [])
    want("override", label, "CUBE_S3_ADDRESSING_STYLE", ["virtual", "path"])
    want("disabled", label, "CUBE_S3_ADDRESSING_STYLE", [])
want("override", "ops", "CUBE_OPS_S3_ADDRESSING_STYLE", ["virtual", "path"])
print("ok: manual credentials retain chart style; later operator env overrides it")

for label in ("volume-s3", "cube-s3lvol"):
    before = component("volume-default", label, "Secret").split("stringData:", 1)[1]
    after = component("volume-virtual", label, "Secret").split("stringData:", 1)[1]
    if before != after:
        raise SystemExit(f"Go addressing settings unexpectedly changed {label} config")
print("ok: volume-s3 and CubeS3lvol configs are unchanged")
PY

expect_fail cubeOps.s3.addressingStyle --set-string cubeOps.s3.addressingStyle=virtual-host
expect_fail controlPlane.artifactStore.s3AddressingStyle --set-string controlPlane.artifactStore.s3AddressingStyle=virtual-host
expect_fail cubeOps.s3.addressingStyle --set cubeOps.s3.addressingStyle=false
expect_fail controlPlane.artifactStore.s3AddressingStyle --set controlPlane.artifactStore.s3AddressingStyle=false
echo "All S3 addressing-style guard tests passed"
