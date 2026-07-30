#!/bin/sh
# Guard: cube-master must use Recreate by default (RWO PVC cannot multi-attach).
# Stateless control-plane Deployments keep RollingUpdate with maxSurge.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

helm template master-strategy-guard "$CHART_DIR" \
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


def find_deploy(component: str) -> str:
    matches = []
    for doc in docs:
        body = f"\n{doc}\n"
        if "\nkind: Deployment\n" not in body:
            continue
        if f"\n    app.kubernetes.io/component: {component}\n" not in body:
            continue
        matches.append(doc)
    if len(matches) != 1:
        raise SystemExit(f"expected one Deployment for {component}, found {len(matches)}")
    return matches[0]


def strategy_type(doc: str) -> str:
    m = re.search(r"(?m)^  strategy:\n(?:    .*\n)*?    type:\s*(\S+)\s*$", doc)
    if not m:
        # Fallback: type directly under strategy
        m = re.search(r"(?m)^  strategy:\n    type:\s*(\S+)\s*$", doc)
    if not m:
        raise SystemExit("missing strategy.type")
    return m.group(1)


def has_max_surge(doc: str, value: str) -> bool:
    return re.search(rf"(?m)^\s+maxSurge:\s*{re.escape(value)}\s*$", doc) is not None


master = find_deploy("master")
master_type = strategy_type(master)
if master_type != "Recreate":
    raise SystemExit(f"cube-master strategy.type must be Recreate, got {master_type!r}")
if has_max_surge(master, "1") or "maxSurge:" in master:
    raise SystemExit("cube-master must not render maxSurge (Recreate omits rollingUpdate)")

api = find_deploy("api")
api_type = strategy_type(api)
if api_type != "RollingUpdate":
    raise SystemExit(f"cube-api strategy.type must remain RollingUpdate, got {api_type!r}")
if not has_max_surge(api, "1"):
    raise SystemExit("cube-api must keep maxSurge: 1 under shared RollingUpdate")

print("ok: cube-master uses Recreate without maxSurge")
print("ok: cube-api keeps RollingUpdate maxSurge: 1")
PY

echo "All master Recreate strategy guard tests passed"
