#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

helm template helm-guard "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "$TMP_DIR/render.yaml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

python3 - "$TMP_DIR/render.yaml" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
docs = text.split("\n---\n")

def pod_doc(name):
    for d in docs:
        if re.search(r"^kind: Pod$", d, re.M) and re.search(rf"^  name: {re.escape(name)}$", d, re.M):
            return d
    raise SystemExit(f"pod {name} not found")

def check_pod(name, placement, container_image_spec):
    d = pod_doc(name)
    if placement == "compute":
        if "cube.tencent.com/cube-node" not in d or "cube.tencent.com/cube-control" in d:
            raise SystemExit(f"{name}: expected computePlacement only")
    elif placement == "control":
        if "cube.tencent.com/cube-control" not in d:
            raise SystemExit(f"{name}: expected controlPlanePlacement")
    else:
        raise SystemExit(f"bad placement {placement}")
    for container, expected_image_val in container_image_spec.items():
        m = re.search(rf"- name: {container}\s*\n\s+image: (\S+)", d)
        if not m:
            raise SystemExit(f"{name}: container {container} missing")
        if expected_image_val not in m.group(1):
            raise SystemExit(f"{name}: container {container} image {m.group(1)} != expected containing {expected_image_val}")
    print(f"OK {name} ({placement}Placement, images match)")

check_pod("helm-guard-cube-health-test", "compute", {"curl": "curlimages/curl"})
check_pod("helm-guard-cube-node-image-test", "compute", {"node-image-assets": "curlimages/curl"})
check_pod("helm-guard-cube-node-runtime-test", "compute", {"node-runtime": "curlimages/curl"})
check_pod("helm-guard-cube-cubemastercli-test", "control", {"cubemastercli": "cubemastercli"})
check_pod("helm-guard-cube-mysql-test", "control", {"mysql": "mysql"})
check_pod("helm-guard-cube-redis-test", "control", {"redis": "redis"})
check_pod("helm-guard-cube-proxy-control-test", "control", {"proxy": "curlimages/curl"})
check_pod("helm-guard-cube-dns-test", "control", {"dns": "curlimages/curl"})

# proxy-control-test must probe /admin/healthz with the admin token from the
# release Secret (not hardcoded).
d = pod_doc("helm-guard-cube-proxy-control-test")
if "/admin/healthz" not in d:
    raise SystemExit("proxy-control-test does not probe admin healthz")
if "CUBE_PROXY_ADMIN_TOKEN" not in d or "secretKeyRef" not in d or "cube-admin-token" not in d:
    raise SystemExit("proxy-control-test does not source the admin token from the release Secret")

# node-runtime-test must mount hostPaths read-only and drop capabilities.
d = pod_doc("helm-guard-cube-node-runtime-test")
if d.count("readOnly: true") < 3:
    raise SystemExit("node-runtime-test hostPath mounts are not all read-only")
if "allowPrivilegeEscalation: false" not in d or "drop:" not in d:
    raise SystemExit("node-runtime-test securityContext missing privilege hardening")

# dns-test must use dnsImage (curlimages/curl) and the getent path.
d = pod_doc("helm-guard-cube-dns-test")
if "getent ahostsv4" not in d:
    raise SystemExit("dns-test does not use getent ahostsv4")

print("helm test placement/image/probe guard passed")
PY
