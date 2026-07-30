#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  output="$1"
  shift
  helm template inplace-guard "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
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

normalize_frozen_template() {
  input="$1"
  output="$2"
  python3 - "$input" "$output" <<'PY'
import pathlib
import sys

lines = pathlib.Path(sys.argv[1]).read_text().splitlines()
start = lines.index("  template:")
lines = lines[start:]
normalized = []
skip_indent = None
for line in lines:
    stripped = line.lstrip()
    indent = len(line) - len(stripped)
    if skip_indent is not None:
        if stripped and indent <= skip_indent:
            skip_indent = None
        else:
            continue
    if stripped.startswith("image:"):
        continue
    # Container resources are allowed to change without failing the guard.
    if stripped == "resources:":
        skip_indent = indent
        continue
    normalized.append(line)
pathlib.Path(sys.argv[2]).write_text("\n".join(normalized) + "\n")
PY
}

render "$TMP_DIR/base.yaml"
render "$TMP_DIR/policy.yaml" \
  --set-string bootstrap.pvmHostKernel.desiredKernelPattern=guard.changed \
  --set-string bootstrap.pvmHostKernel.bootArgs=guard_arg=1 \
  --set-string cubeNodeBootstrap.prepGeneration=guard-2 \
  --set bootstrap.pvmHostKernel.startupGate.reconcileIntervalSeconds=17
render "$TMP_DIR/pvm-disabled.yaml" \
  --set bootstrap.pvmHostKernel.enabled=false

extract_big_pod "$TMP_DIR/base.yaml" "$TMP_DIR/base-node.yaml"
extract_big_pod "$TMP_DIR/policy.yaml" "$TMP_DIR/policy-node.yaml"
extract_big_pod "$TMP_DIR/pvm-disabled.yaml" "$TMP_DIR/pvm-disabled-node.yaml"
normalize_frozen_template "$TMP_DIR/base-node.yaml" "$TMP_DIR/base-frozen.yaml"

# PVM / bootArgs / prepGeneration / gate must not touch Big Pod at all.
diff -u "$TMP_DIR/base-node.yaml" "$TMP_DIR/policy-node.yaml"
diff -u "$TMP_DIR/base-node.yaml" "$TMP_DIR/pvm-disabled-node.yaml"


assert_recreate_change_detected() {
  name="$1"
  shift
  render "$TMP_DIR/${name}.yaml" "$@"
  extract_big_pod "$TMP_DIR/${name}.yaml" "$TMP_DIR/${name}-node.yaml"
  normalize_frozen_template "$TMP_DIR/${name}-node.yaml" "$TMP_DIR/${name}-frozen.yaml"
  if diff -q "$TMP_DIR/base-frozen.yaml" "$TMP_DIR/${name}-frozen.yaml" >/dev/null; then
    echo "expected maintenance-only value '${name}' to change the frozen template" >&2
    exit 1
  fi
}

assert_recreate_change_detected pod-annotation \
  --set-string cubeNode.podAnnotations.checksum/config=changed
assert_recreate_change_detected custom-env \
  --set-string cubeNode.env[0].name=GUARD_TEST \
  --set-string cubeNode.env[0].value=changed
assert_recreate_change_detected timezone \
  --set-string global.timezone=UTC
assert_recreate_change_detected network \
  --set-string cubeNode.network.ethName=eth9
assert_recreate_change_detected egress \
  --set cubeEgress.enabled=false
echo "Big Pod template stability guard passed"
