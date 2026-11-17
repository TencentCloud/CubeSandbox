#!/bin/sh
# Guard: the proxy Deployment resolves NODE_IP / CUBE_PROXY_NODE_IP from
# status.hostIP. CubeProxy compares its caller host IP against the sandbox's
# HostIP to pick the direct same-host path; feeding it the Pod IP makes the
# comparison always false, so single-node deployments route every request
# through the cross-host HostPort path and fail with 502
# (see CubeProxy/lua/sandbox_backend.lua get_caller_host_ip / same_host).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

helm template node-ip "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "$TMP_DIR/rendered.yaml"

python3 - "$TMP_DIR/rendered.yaml" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
for doc in text.split("\n---\n"):
    if re.search(r"^kind: Deployment$", doc, re.M) and "app.kubernetes.io/component: cube-proxy" in doc:
        break
else:
    raise SystemExit("cube-proxy Deployment not found in rendered output")

def env_source(doc_text, name):
    m = re.search(r"name: " + re.escape(name) + r"\s*\n(.*?)\n\s*- name:", doc_text, re.S)
    if not m:
        raise SystemExit(name + " env entry not found on proxy Deployment")
    return m.group(1)

for name in ("NODE_IP", "CUBE_PROXY_NODE_IP"):
    block = env_source(doc, name)
    if "status.hostIP" not in block or "valueFrom" not in block:
        raise SystemExit(
            name + " must resolve from status.hostIP via valueFrom, got: " + block.strip()
        )
    print(name + " OK: status.hostIP")

pod_block = env_source(doc, "POD_IP")
if "status.podIP" not in pod_block:
    raise SystemExit("POD_IP must stay on status.podIP, got: " + pod_block.strip())
print("POD_IP OK: status.podIP (unchanged)")
PY

echo "Proxy node IP guard passed"
