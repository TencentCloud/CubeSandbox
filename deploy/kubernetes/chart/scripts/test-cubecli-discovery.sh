#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="${CHART_DIR:-$(dirname "$SCRIPT_DIR")}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  output="$1"
  shift
  helm template cubecli-discovery "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

extract_daemonset() {
  component="$1"
  input="$2"
  output="$3"
  python3 - "$component" "$input" "$output" <<'PY'
import pathlib
import re
import sys

component = sys.argv[1]
documents = pathlib.Path(sys.argv[2]).read_text().split("\n---\n")
matches = [
    doc for doc in documents
    if "\nkind: DaemonSet\n" in f"\n{doc}\n"
    and f"\n    app.kubernetes.io/component: {component}\n" in f"\n{doc}\n"
    and re.search(r"(?m)^apiVersion:\s*apps/v1\s*$", doc)
]
if len(matches) != 1:
    raise SystemExit(f"expected one {component} DaemonSet, found {len(matches)}")
pathlib.Path(sys.argv[3]).write_text(matches[0].strip() + "\n")
PY
}

render "$TMP_DIR/default.yaml"
extract_daemonset cube-node "$TMP_DIR/default.yaml" "$TMP_DIR/default-node.yaml"
grep -q '^        kubectl.kubernetes.io/default-container: cubelet$' \
  "$TMP_DIR/default-node.yaml" || {
    echo "cube-node must default kubectl exec to the cubelet container" >&2
    exit 1
  }

printf '%s\n' \
  'cubeNode:' \
  '  podAnnotations:' \
  '    kubectl.kubernetes.io/default-container: network-agent' \
  > "$TMP_DIR/override.yaml"
render "$TMP_DIR/override-rendered.yaml" -f "$TMP_DIR/override.yaml"
extract_daemonset cube-node "$TMP_DIR/override-rendered.yaml" "$TMP_DIR/override-node.yaml"
grep -q '^        kubectl.kubernetes.io/default-container: network-agent$' \
  "$TMP_DIR/override-node.yaml" || {
    echo "cubeNode.podAnnotations must be able to override the default container" >&2
    exit 1
  }

printf '%s\n' \
  'cubeNode:' \
  '  podAnnotations:' \
  '    example.com/operator-note: retained' \
  > "$TMP_DIR/unrelated-override.yaml"
render "$TMP_DIR/unrelated-rendered.yaml" -f "$TMP_DIR/unrelated-override.yaml"
extract_daemonset cube-node "$TMP_DIR/unrelated-rendered.yaml" "$TMP_DIR/unrelated-node.yaml"
grep -q '^        kubectl.kubernetes.io/default-container: cubelet$' \
  "$TMP_DIR/unrelated-node.yaml" || {
    echo "unrelated cubeNode.podAnnotations must preserve the cubelet default" >&2
    exit 1
  }
grep -q '^        example.com/operator-note: retained$' \
  "$TMP_DIR/unrelated-node.yaml" || {
    echo "custom cubeNode.podAnnotations must be preserved" >&2
    exit 1
  }

echo "CubeCLI Helm discovery guard passed"
