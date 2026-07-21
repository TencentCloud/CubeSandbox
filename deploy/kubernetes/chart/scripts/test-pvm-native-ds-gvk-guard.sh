#!/bin/sh
# T1/T2: PVM is native apps/v1 DaemonSet; other compute plane remain ADS.
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

pvm = find_ds("cube-node-pvm")
if api_version(pvm) != "apps/v1":
    raise SystemExit(f"PVM apiVersion must be apps/v1, got {api_version(pvm)!r}")
if "rollingUpdateType" in pvm:
    raise SystemExit("PVM DaemonSet must not contain rollingUpdateType")

for component, expect_type in (
    ("cube-node", "InPlaceIfPossible"),
    ("cube-node-bootstrap", "Standard"),
    ("cube-node-installer", "Standard"),
):
    doc = find_ds(component)
    if api_version(doc) != "apps.kruise.io/v1beta1":
        raise SystemExit(
            f"{component} apiVersion must stay apps.kruise.io/v1beta1, got {api_version(doc)!r}"
        )
    if f"rollingUpdateType: {expect_type}" not in doc:
        raise SystemExit(f"{component} missing rollingUpdateType: {expect_type}")

# Automatic ADS→native migrate Hook was removed; Chart must not render it.
for needle in (
    "pvm-ads-migrate",
    "pvm-ads-to-native-migrate",
    "pvm-ads-migration",
):
    if needle in text:
        raise SystemExit(f"migration hook must not be rendered (found {needle!r})")

print("ok: PVM native DS + three ADS GVK guard")
print("ok: no PVM ADS→native migration hook rendered")
PY

# T4: stale rollingUpdateType must fail validate
if helm template pvm-gvk-guard "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set-string cubeNodePvm.updateStrategy.rollingUpdate.rollingUpdateType=Standard \
  >"$TMP_DIR/bad.yaml" 2>"$TMP_DIR/bad.err"; then
  echo "FAIL: expected validate failure for rollingUpdateType" >&2
  exit 1
fi
grep -q 'rollingUpdateType was removed' "$TMP_DIR/bad.err" \
  || { echo "FAIL: missing validate message"; cat "$TMP_DIR/bad.err" >&2; exit 1; }
echo "ok: validate rejects cubeNodePvm rollingUpdateType"

echo "All PVM native DaemonSet GVK guard tests passed"
