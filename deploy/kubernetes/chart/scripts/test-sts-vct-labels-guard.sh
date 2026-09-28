#!/bin/sh
# Guard: StatefulSet volumeClaimTemplates must not carry cube.labels.
# helm.sh/chart and app.kubernetes.io/version change on every Chart.yaml bump
# and Kubernetes forbids mutating volumeClaimTemplates.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

helm template sts-vct-labels-guard "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "$TMP_DIR/rendered.yaml"

python3 - "$TMP_DIR/rendered.yaml" <<'PY'
import pathlib
import sys

text = pathlib.Path(sys.argv[1]).read_text()
docs = [d for d in text.split("\n---\n") if d.strip()]

FORBIDDEN = (
    "helm.sh/chart:",
    "app.kubernetes.io/version:",
    "app.kubernetes.io/managed-by:",
)
REQUIRED = (
    "app.kubernetes.io/name:",
    "app.kubernetes.io/instance:",
    "app.kubernetes.io/component:",
)
EXPECT = {
    "redis": "redis-data",
    "mysql": "mysql-data",
    "minio": "minio-data",
}


def find_sts(component: str) -> str:
    matches = []
    for doc in docs:
        body = f"\n{doc}\n"
        if "\nkind: StatefulSet\n" not in body:
            continue
        if f"\n    app.kubernetes.io/component: {component}\n" not in body:
            continue
        matches.append(doc)
    if len(matches) != 1:
        raise SystemExit(f"expected one StatefulSet for {component}, found {len(matches)}")
    return matches[0]


def vct_block(doc: str, claim: str) -> str:
    lines = doc.splitlines()
    start = None
    for i, line in enumerate(lines):
        if line == "  volumeClaimTemplates:":
            start = i
            break
    if start is None:
        raise SystemExit("missing volumeClaimTemplates")
    block = []
    for line in lines[start + 1 :]:
        if line and not line.startswith(" "):
            break
        if line.startswith("  ") and not line.startswith("   ") and block:
            break
        block.append(line)
    text = "\n".join(block)
    if f"name: {claim}" not in text:
        raise SystemExit(f"volumeClaimTemplates missing claim {claim}")
    return text


for component, claim in EXPECT.items():
    vct = vct_block(find_sts(component), claim)
    for needle in FORBIDDEN:
        if needle in vct:
            raise SystemExit(
                f"{component} volumeClaimTemplates must not include {needle.strip()} "
                "(Chart version labels are STS-immutable)"
            )
    for needle in REQUIRED:
        if needle not in vct:
            raise SystemExit(f"{component} volumeClaimTemplates missing {needle.strip()}")
    print(f"ok: {component} volumeClaimTemplates use stable labels")

print("ok: StatefulSet volumeClaimTemplates stay version-independent")
PY

echo "All StatefulSet volumeClaimTemplates label guard tests passed"
