#!/bin/sh
# Guard: the Go artifact and warehouse clients support an explicit S3
# addressing style while leaving legacy booleans and volume clients alone.
set -eu

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Pin MinIO credentials so volume Secret comparisons only track settings.
helm_template() {
  helm template s3-addressing "$CHART_DIR" \
    --set-string mysql.password=test --set-string mysql.rootPassword=test \
    --set-string redis.password=test --set-string minio.rootPassword=test-minio-password "$@"
}

render() {
  name="$1"
  shift
  helm_template "$@" > "$TMP_DIR/$name.yaml"
}

expect_fail() {
  setting="$1"
  shift
  if helm_template \
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
render null-ops-s3 --set cubeOps.s3=null
render legacy --set controlPlane.artifactStore.s3Backed=true
render whitespace --set controlPlane.artifactStore.s3Backed=true \
  --set-string 'controlPlane.artifactStore.s3AddressingStyle=   ' --set-string 'cubeOps.s3.addressingStyle=   '
render legacy-false --set controlPlane.artifactStore.s3Backed=true \
  --set cubeOps.s3.pathStyle=false --set minio.enabled=false \
  --set-string volumeS3.endpoint=https://storage.example.test \
  --set-string volumeS3.accessKeyId=test-ak --set-string volumeS3.secretAccessKey=test-sk \
  --set-string volumeS3.bucket=artifacts \
  --set volumeS3.pathStyle=false

# Use the same external endpoint for all three style renders.
external_s3() {
  name="$1"
  shift
  render "$name" --set minio.enabled=false --set volumeS3.pathStyle=true \
    --set-string volumeS3.endpoint=https://storage.example.test \
    --set-string volumeS3.accessKeyId=test-ak --set-string volumeS3.secretAccessKey=test-sk \
    --set-string volumeS3.bucket=artifacts "$@"
}

for style in auto path virtual; do
  external_s3 "$style" --set controlPlane.artifactStore.s3Backed=true \
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
render style-from-secret -f "$TMP_DIR/manual-values.yaml" \
  --set-string 'global.env[4].name=CUBE_S3_ADDRESSING_STYLE' \
  --set-string 'global.env[4].valueFrom.secretKeyRef.name=settings' --set-string 'global.env[4].valueFrom.secretKeyRef.key=style' \
  --set-string cubeOps.s3.addressingStyle=virtual --set-string 'cubeOps.env[0].name=CUBE_OPS_S3_ADDRESSING_STYLE' \
  --set-string 'cubeOps.env[0].valueFrom.secretKeyRef.name=settings' --set-string 'cubeOps.env[0].valueFrom.secretKeyRef.key=style'
render service-override --set controlPlane.artifactStore.s3Backed=true \
  --set-string controlPlane.artifactStore.s3AddressingStyle=virtual \
  --set-string 'global.env[0].name=CUBE_S3_ADDRESSING_STYLE' --set-string 'global.env[0].value=virtual' \
  --set-string 'controlPlane.master.env[0].name=CUBE_S3_ADDRESSING_STYLE' \
  --set-string 'controlPlane.master.env[0].value= PaTh ' \
  --set-string 'controlPlane.templateCenter.env[0].name=CUBE_S3_ADDRESSING_STYLE' \
  --set-string 'controlPlane.templateCenter.env[0].value= AuTo '
external_s3 normalized --set controlPlane.artifactStore.s3Backed=true \
  --set-string 'controlPlane.artifactStore.s3AddressingStyle= ViRtUaL ' \
  --set-string 'cubeOps.s3.addressingStyle= PaTh '
render disabled --set-string controlPlane.artifactStore.s3AddressingStyle=virtual

external_s3 volume-default --set cubeS3lvol.enabled=true
external_s3 volume-virtual --set cubeS3lvol.enabled=true \
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
    return [value.strip().strip("\"'") for value in re.findall(
        r"name: " + re.escape(key) + r'\n\s+value: ([^\n]*)', doc)]

def want(name, label, key, expected):
    actual = env(name, label, key)
    if actual != expected:
        raise SystemExit(f"{name}/{label}: {key}={actual!r}, want {expected!r}")

for name in ("default", "null-ops-s3", "legacy", "legacy-false", "whitespace"):
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
    want("override", label, "CUBE_S3_ADDRESSING_STYLE", ["path"])
    want("disabled", label, "CUBE_S3_ADDRESSING_STYLE", [])
want("override", "ops", "CUBE_OPS_S3_ADDRESSING_STYLE", ["path"])
want("service-override", "master", "CUBE_S3_ADDRESSING_STYLE", ["virtual", " PaTh "])
want("service-override", "templatecenter", "CUBE_S3_ADDRESSING_STYLE", ["virtual", " AuTo "])
for label in ("master", "templatecenter"):
    want("normalized", label, "CUBE_S3_ADDRESSING_STYLE", ["virtual"])
want("normalized", "ops", "CUBE_OPS_S3_ADDRESSING_STYLE", ["path"])
for label, key in (("master", "CUBE_S3_ADDRESSING_STYLE"), ("templatecenter", "CUBE_S3_ADDRESSING_STYLE"),
                   ("ops", "CUBE_OPS_S3_ADDRESSING_STYLE")):
    doc = component("style-from-secret", label)
    if doc.count("name: " + key + "\n") != 1 or not re.search(r"name: " + key + r"\n\s+valueFrom:", doc):
        raise SystemExit(f"{label}: expected exactly one {key} from the operator Secret")
print("ok: manual credentials retain chart style; service env follows global env")

for label in ("volume-s3", "cube-s3lvol"):
    before = component("volume-default", label, "Secret").split("stringData:", 1)[1]
    after = component("volume-virtual", label, "Secret").split("stringData:", 1)[1]
    if before != after:
        raise SystemExit(f"Go addressing settings unexpectedly changed {label} config")
print("ok: volume-s3 and CubeS3lvol configs are unchanged")
PY

expect_fail 'global.env CUBE_S3_ADDRESSING_STYLE' --set controlPlane.artifactStore.s3Backed=true \
  --set-string 'global.env[0].name=CUBE_S3_ADDRESSING_STYLE' --set-string 'global.env[0].value=vritual'
expect_fail 'cubeOps.env CUBE_OPS_S3_ADDRESSING_STYLE' \
  --set-string 'cubeOps.env[0].name=CUBE_OPS_S3_ADDRESSING_STYLE' --set-string 'cubeOps.env[0].value=vritual'
for service in master templateCenter; do
  expect_fail "controlPlane.$service.env CUBE_S3_ADDRESSING_STYLE" --set controlPlane.artifactStore.s3Backed=true \
    --set-string "controlPlane.$service.env[0].name=CUBE_S3_ADDRESSING_STYLE" \
    --set-string "controlPlane.$service.env[0].value=vritual"
done
expect_fail cubeOps.s3.addressingStyle --set-string cubeOps.s3.addressingStyle=virtual-host
expect_fail controlPlane.artifactStore.s3AddressingStyle --set-string controlPlane.artifactStore.s3AddressingStyle=virtual-host
expect_fail cubeOps.s3.addressingStyle --set cubeOps.s3.addressingStyle=false
expect_fail controlPlane.artifactStore.s3AddressingStyle --set controlPlane.artifactStore.s3AddressingStyle=false
for setting in controlPlane.artifactStore.s3AddressingStyle cubeOps.s3.addressingStyle; do
  if helm_template --set minio.enabled=true \
      --set controlPlane.artifactStore.s3Backed=true --set-string "$setting=virtual" \
      >/dev/null 2>"$TMP_DIR/minio.err"; then
    echo "bundled MinIO unexpectedly accepted $setting=virtual" >&2
    exit 1
  fi
  grep -F "$setting: virtual addressing requires" "$TMP_DIR/minio.err" >/dev/null
done
for endpoint in ' http://s3-addressing-cube-minio.default.svc.cluster.local:9000//' \
    's3-addressing-cube-minio.default.svc.cluster.local:9000' \
    'HTTP://S3-ADDRESSING-CUBE-MINIO.DEFAULT.SVC.CLUSTER.LOCAL:9000' \
    'http://s3-addressing-cube-minio:9000' \
    'http://s3-addressing-cube-minio.default.svc.:9000' \
    'http://s3-addressing-cube-minio:' \
    'http://s3-addressing-cube-minio:9000?foo=bar' \
    'http://user:pass@s3-addressing-cube-minio:9000'; do
  if helm_template --set-string cubeOps.s3.addressingStyle=virtual \
      --set-string "cubeOps.s3.endpoint=$endpoint" >/dev/null 2>"$TMP_DIR/minio.err"; then
    echo "bundled MinIO endpoint unexpectedly accepted: $endpoint" >&2
    exit 1
  fi
  grep -F 'cubeOps.s3.addressingStyle: virtual addressing requires' "$TMP_DIR/minio.err" >/dev/null
done
expect_builtin_minio_failure() {
  if helm_template --set-string cubeOps.s3.addressingStyle=virtual \
      "$@" >/dev/null 2>"$TMP_DIR/minio.err"; then
    echo "bundled MinIO unexpectedly accepted virtual addressing ($*)" >&2
    exit 1
  fi
  grep -Fq 'cubeOps.s3.addressingStyle: virtual addressing requires' "$TMP_DIR/minio.err"
}
expect_builtin_minio_failure --set-string global.clusterDomain=cluster.local. \
  --set-string cubeOps.s3.endpoint=http://s3-addressing-cube-minio.default.svc.cluster.local:9000
expect_builtin_minio_failure --set-string 'cubeOps.env[0].name=CUBE_OPS_STORE_BACKEND' \
  --set-string 'cubeOps.env[0].value='
expect_builtin_minio_failure --set-string 'cubeOps.env[0].name=CUBE_OPS_STORE_BACKEND' \
  --set-string 'cubeOps.env[0].valueFrom.secretKeyRef.name=settings' \
  --set-string 'cubeOps.env[0].valueFrom.secretKeyRef.key=backend'
render builtin-fs --set-string cubeOps.s3.addressingStyle=virtual \
  --set-string 'cubeOps.env[0].name=CUBE_OPS_STORE_BACKEND' --set-string 'cubeOps.env[0].value=fs'
if helm_template --set-string cubeOps.s3.addressingStyle=virtual \
    --set-string cubeOps.s3.endpoint=https://warehouse.example.test \
    --set-string cubeOps.s3.nodeEndpoint=http://s3-addressing-cube-minio:9000 \
    >/dev/null 2>"$TMP_DIR/minio.err"; then
  echo "bundled MinIO download endpoint unexpectedly accepted" >&2
  exit 1
fi
grep -F 'cubeOps.s3.addressingStyle: virtual addressing requires' "$TMP_DIR/minio.err" >/dev/null
render builtin-override --set minio.enabled=true --set controlPlane.artifactStore.s3Backed=true \
  --set-string controlPlane.artifactStore.s3AddressingStyle=virtual \
  --set-string 'global.env[0].name=CUBE_S3_ADDRESSING_STYLE' --set-string 'global.env[0].value=path' \
  --set-string cubeOps.s3.addressingStyle=virtual --set-string cubeOps.s3.endpoint=https://warehouse.example.test
echo "All S3 addressing-style guard tests passed"
