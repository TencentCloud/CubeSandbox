#!/bin/sh
# Guard: all compute-plane DaemonSets are native apps/v1 (no rollingUpdateType).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

helm template pvm-gvk-guard "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "$TMP_DIR/rendered.yaml"

python3 - "$TMP_DIR/rendered.yaml" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
docs = [d for d in text.split("\n---\n") if d.strip()]

def find_ds(component: str):
    matches = []
    for doc in docs:
        body = f"\n{doc}\n"
        if "\nkind: DaemonSet\n" not in body:
            continue
        if f"\n    app.kubernetes.io/component: {component}\n" not in body:
            continue
        matches.append(doc)
    if len(matches) != 1:
        raise SystemExit(f"expected one DaemonSet for {component}, found {len(matches)}")
    return matches[0]

def api_version(doc: str) -> str:
    m = re.search(r"(?m)^apiVersion:\s*(\S+)\s*$", doc)
    if not m:
        raise SystemExit("missing apiVersion")
    return m.group(1)

for component in (
    "cube-node",
    "cube-node-bootstrap",
    "cube-node-installer",
    "cube-node-pvm",
):
    doc = find_ds(component)
    if api_version(doc) != "apps/v1":
        raise SystemExit(f"{component} apiVersion must be apps/v1, got {api_version(doc)!r}")
    if "rollingUpdateType" in doc:
        raise SystemExit(f"{component} must not contain rollingUpdateType")

node = find_ds("cube-node")
if "initContainers:" not in node or "name: wait-node-prep" not in node:
    raise SystemExit("cube-node must have wait-node-prep as initContainer")
# wait must not appear as a long-running container alongside network-agent
containers_idx = node.find("\n      containers:")
init_idx = node.find("\n      initContainers:")
if containers_idx < 0 or init_idx < 0 or containers_idx < init_idx:
    raise SystemExit("cube-node initContainers must precede containers")
run_section = node[containers_idx:]
if re.search(r"(?m)^\s+- name: wait-node-prep\s*$", run_section):
    raise SystemExit("wait-node-prep must not be a run container")

print("ok: all compute DaemonSets are native apps/v1")
print("ok: wait-node-prep is initContainer only")
PY

# stale rollingUpdateType on any compute DS must fail validate
for key in cubeNode cubeNodeBootstrap cubeNodeInstaller cubeNodePvm; do
  if helm template pvm-gvk-guard "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    --set-string "${key}.updateStrategy.rollingUpdate.rollingUpdateType=Standard" \
    >"$TMP_DIR/bad.yaml" 2>"$TMP_DIR/bad.err"; then
    echo "FAIL: expected validate failure for ${key} rollingUpdateType" >&2
    exit 1
  fi
  grep -q 'rollingUpdateType was removed' "$TMP_DIR/bad.err" \
    || { echo "FAIL: missing validate message for ${key}"; cat "$TMP_DIR/bad.err" >&2; exit 1; }
  echo "ok: validate rejects ${key} rollingUpdateType"
done

echo "All native DaemonSet GVK guard tests passed"
