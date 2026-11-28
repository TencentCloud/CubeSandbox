#!/usr/bin/env bash
# Guard that the cube-node Big Pod configures anchor-backed mount propagation:
# - cubelet sets CUBELET_DEPLOYED_IN_BIG_POD="true" and CUBELET_SHARED_MNT_PATHS
# - cubelet omits mountPropagation: Bidirectional on dataplane volumes so
#   per-sandbox mounts never propagate into host PID 1 / systemd
# - cube-juicefs retains Bidirectional when enabled so its single static FUSE
#   mount propagates from host into the anchor (H -> C)
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  output="$1"
  shift
  helm template node-mntns "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

assert_big_pod_mntns() {
  input="$1"
  label="$2"
  expected_paths="$3"
  python3 - "$input" "$label" "$expected_paths" <<'PY'
import pathlib
import re
import sys

documents = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
label = sys.argv[2]
expected_paths = sys.argv[3]
matches = [
    doc for doc in documents
    if re.search(r"(?m)^kind:\s*DaemonSet\s*$", doc)
    and re.search(r"(?m)^\s*app\.kubernetes\.io/component: cube-node\s*$", doc)
]
if len(matches) != 1:
    raise SystemExit(f"{label}: expected one cube-node DaemonSet, found {len(matches)}")

doc = matches[0]
if not re.search(r"(?m)^\s*hostPID:\s*true\s*$", doc):
    raise SystemExit(f"{label}: cube-node DaemonSet must run with hostPID: true")

containers_section = re.search(
    r"(?m)^      containers:\n(.*?)(?=^      volumes:|\Z)", doc, re.S
)
if not containers_section:
    raise SystemExit(f"{label}: containers section not found")

blocks = re.split(r"(?m)^        - name: ", containers_section.group(1))[1:]
seen_cubelet = False
for block in blocks:
    name = block.splitlines()[0].strip()
    has_big_pod = re.search(
        r'(?m)^\s+- name: CUBELET_DEPLOYED_IN_BIG_POD\n\s+value: "true"$',
        block,
    ) is not None
    shared_paths_match = re.search(
        r'(?m)^\s+- name: CUBELET_SHARED_MNT_PATHS\n\s+value: "([^"]+)"$',
        block,
    )
    has_bidirectional = re.search(r"(?m)^\s+mountPropagation:\s*Bidirectional\s*$", block) is not None
    if name == "cubelet":
        seen_cubelet = True
        if not has_big_pod:
            raise SystemExit(
                f'{label}: cubelet container must set CUBELET_DEPLOYED_IN_BIG_POD="true"'
            )
        if not shared_paths_match or shared_paths_match.group(1) != expected_paths:
            got = shared_paths_match.group(1) if shared_paths_match else "<missing>"
            raise SystemExit(
                f"{label}: cubelet CUBELET_SHARED_MNT_PATHS={got!r}, want {expected_paths!r}"
            )
        if has_bidirectional:
            raise SystemExit(
                f"{label}: cubelet container must not set mountPropagation: Bidirectional"
            )
    else:
        if has_big_pod:
            raise SystemExit(
                f"{label}: non-cubelet container {name!r} must not set CUBELET_DEPLOYED_IN_BIG_POD"
            )
        if shared_paths_match:
            raise SystemExit(
                f"{label}: non-cubelet container {name!r} must not set CUBELET_SHARED_MNT_PATHS"
            )

if not seen_cubelet:
    raise SystemExit(f"{label}: cubelet container not found")
print(f"ok: {label} configures anchor-backed Big Pod mntns on cubelet without Bidirectional")
PY
}

DEFAULT_PATHS="/data/cubelet:/data/cube-shared:/data/shared:/data/cube-shim:/tmp/cube"

render "$TMP_DIR/default.yaml"
assert_big_pod_mntns "$TMP_DIR/default.yaml" default "$DEFAULT_PATHS"

render "$TMP_DIR/podnet.yaml" \
  --set cubeNode.hostNetwork=false \
  --set bootstrap.nodeInit.checkHostPorts=false
assert_big_pod_mntns "$TMP_DIR/podnet.yaml" pod-network "$DEFAULT_PATHS"

render "$TMP_DIR/s3lvol.yaml" --set cubeS3lvol.enabled=true
assert_big_pod_mntns "$TMP_DIR/s3lvol.yaml" s3lvol "$DEFAULT_PATHS"

render "$TMP_DIR/custom-paths.yaml" \
  --set hostPaths.dataCubelet=/mnt/ssd/cubelet \
  --set hostPaths.dataCubeShared=/mnt/ssd/cube-shared
assert_big_pod_mntns "$TMP_DIR/custom-paths.yaml" custom-paths \
  "/mnt/ssd/cubelet:/mnt/ssd/cube-shared:/data/shared:/data/cube-shim:/tmp/cube"

REPO_ROOT="$(cd "$CHART_DIR/../../.." && pwd)"
grep -Fq 'ENTRYPOINT ["/usr/local/bin/cube-mntns-init", "--", "/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]' \
  "$REPO_ROOT/Cubelet/Dockerfile" || {
  echo "FAIL: Cubelet/Dockerfile must run cube-mntns-init ahead of tini so container PID 1 enters M_cubelet" >&2
  exit 1
}

echo "ok: cubelet Big Pod mntns guard passed"
