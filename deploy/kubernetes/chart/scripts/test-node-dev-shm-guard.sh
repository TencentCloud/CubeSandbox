#!/bin/sh
# cubelet mounts the host /dev/shm.
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
  grep -q "$needle" "$err_file" || {
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
    if re.search(r"(?m)^kind:\s*DaemonSet\s*$", doc)
    and re.search(r"(?m)^\s*app\.kubernetes\.io/component: cube-node\s*$", doc)
]
if len(matches) != 1:
    raise SystemExit(f"expected one cube-node DaemonSet, found {len(matches)}")
pathlib.Path(sys.argv[2]).write_text(matches[0] + "\n")
PY
}

assert_cubelet_dev_shm() {
  pod="$1"
  label="$2"
  host_path="$3"
  python3 - "$pod" "$label" "$host_path" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
label, host_path = sys.argv[2], sys.argv[3]

cubelet = re.search(r"(?m)^        - name: cubelet\n(.*?)(?=^        - name: )", text, re.S)
if not cubelet:
    raise SystemExit(f"{label}: cubelet container not found")
body = cubelet.group(1)
if not re.search(r"- name: dev-shm\n\s+mountPath: /dev/shm\n", body):
    raise SystemExit(f"{label}: cubelet must mount dev-shm at /dev/shm")

volume = re.search(
    r"(?m)^        - name: dev-shm\n          hostPath:\n            path: (.+)\n            type: Directory\s*$",
    text,
)
if not volume:
    raise SystemExit(f"{label}: dev-shm volume must be a hostPath Directory")
if volume.group(1).strip().strip('"') != host_path:
    raise SystemExit(
        f"{label}: dev-shm hostPath is {volume.group(1)!r}, want {host_path}"
    )
print(f"ok: {label} cubelet mounts {host_path} at /dev/shm")
PY
}

helm template dev-shm-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
extract_big_pod "$TMP_DIR/default.yaml" "$TMP_DIR/default-node.yaml"
assert_cubelet_dev_shm "$TMP_DIR/default-node.yaml" default /dev/shm

helm template dev-shm-podnet "$CHART_DIR" $COMMON_SETS \
  --set cubeNode.hostNetwork=false \
  --set bootstrap.nodeInit.checkHostPorts=false \
  > "$TMP_DIR/podnet.yaml"
extract_big_pod "$TMP_DIR/podnet.yaml" "$TMP_DIR/podnet-node.yaml"
assert_cubelet_dev_shm "$TMP_DIR/podnet-node.yaml" pod-network /dev/shm

helm template dev-shm-s3lvol "$CHART_DIR" $COMMON_SETS \
  --set cubeS3lvol.enabled=true \
  > "$TMP_DIR/s3lvol.yaml"
extract_big_pod "$TMP_DIR/s3lvol.yaml" "$TMP_DIR/s3lvol-node.yaml"
assert_cubelet_dev_shm "$TMP_DIR/s3lvol-node.yaml" s3lvol /dev/shm
python3 - "$TMP_DIR/s3lvol-node.yaml" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
sidecar = re.search(r"(?m)^        - name: cube-s3lvol\n(.*?)(?=^        - name: |\Z)", text, re.S)
if not sidecar:
    raise SystemExit("cube-s3lvol container not found")
if not re.search(r"- name: s3lvol-shm\n\s+mountPath: /dev/shm\n", sidecar.group(1)):
    raise SystemExit("cube-s3lvol must keep its own /dev/shm mount")
if not re.search(
    r"(?m)^        - name: s3lvol-shm\n          hostPath:\n            path: /dev/shm/cube-s3lvol\n            type: DirectoryOrCreate\s*$",
    text,
):
    raise SystemExit("s3lvol-shm must stay on hostPath /dev/shm/cube-s3lvol")
print("ok: cube-s3lvol keeps s3lvol-shm")
PY

helm template dev-shm-custom "$CHART_DIR" $COMMON_SETS \
  --set-string hostPaths.devShm=/run/cube-shm \
  > "$TMP_DIR/custom.yaml"
extract_big_pod "$TMP_DIR/custom.yaml" "$TMP_DIR/custom-node.yaml"
assert_cubelet_dev_shm "$TMP_DIR/custom-node.yaml" custom /run/cube-shm

expect_fail dev-shm-empty "$TMP_DIR/empty.err" "hostPaths.devShm" \
  --set-string hostPaths.devShm=
expect_fail dev-shm-relative "$TMP_DIR/relative.err" "hostPaths.devShm" \
  --set-string hostPaths.devShm=dev/shm
expect_fail dev-shm-dotdot "$TMP_DIR/dotdot.err" "hostPaths.devShm" \
  --set-string hostPaths.devShm=/dev/shm/../other
expect_fail dev-shm-s3lvol-overlap "$TMP_DIR/overlap.err" "cube-s3lvol" \
  --set cubeS3lvol.enabled=true \
  --set-string hostPaths.devShm=/dev/shm/cube-s3lvol

echo "ok: cubelet host /dev/shm mount guard passed"
