#!/bin/sh
# Guard: helm test pods schedule across topologies, proxy probes admin
# healthz, DNS uses getent ahostsv4, and node-runtime-test runs as root
# against read-only hostPaths.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

render() {
  output="$1"
  shift
  helm template helm-guard "$CHART_DIR" $COMMON_SETS "$@" > "$output"
}

python_check() {
  python3 - "$1" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
docs = text.split("\n---\n")

TEST_PLACEMENT_PODS = (
    "helm-guard-cube-health-test",
    "helm-guard-cube-cubemastercli-test",
    "helm-guard-cube-cubeopscli-test",
    "helm-guard-cube-mysql-test",
    "helm-guard-cube-redis-test",
    "helm-guard-cube-proxy-control-test",
    "helm-guard-cube-dns-test",
    "helm-guard-cube-node-image-test",
)

RUNTIME_TEST_POD = "helm-guard-cube-node-runtime-test"


def pod_name(doc):
    m = re.search(r"^  name: (.+)$", doc, re.M)
    return m.group(1).strip() if m else None


def is_hook_test_pod(doc):
    return bool(
        re.search(r"^kind: Pod$", doc, re.M)
        and re.search(r'(?m)^\s+"helm\.sh/hook": test\s*$', doc)
    )


def hook_test_pods():
    names = []
    for d in docs:
        if not is_hook_test_pod(d):
            continue
        name = pod_name(d)
        if name:
            names.append(name)
    return names


def pod_doc(name, required=True):
    for d in docs:
        if re.search(r"^kind: Pod$", d, re.M) and re.search(
            rf"^  name: {re.escape(name)}$", d, re.M
        ):
            return d
    if required:
        raise SystemExit(f"pod {name} not found")
    return None


def spec_top_level_key(doc, key):
    return bool(re.search(rf"(?m)^  {re.escape(key)}:", doc))


def spec_toleration_keys(doc):
    m = re.search(r"(?ms)^  tolerations:\n((?:    .+\n)*)", doc)
    if not m:
        return set()
    keys = set()
    for line in m.group(1).splitlines():
        mm = re.match(r"^\s+-?\s*key:\s*[\"']?([^\"'\s]+)[\"']?\s*$", line)
        if mm:
            keys.add(mm.group(1))
    return keys


def has_node_selector_label(doc, label):
    return bool(
        re.search(rf"(?m)^  nodeSelector:\n(?:    .+\n)*    {re.escape(label)}:", doc)
    )


def uncommented_has(doc, needle):
    return any(
        needle in line and not line.lstrip().startswith("#")
        for line in doc.splitlines()
    )


def check_test_placement(name):
    d = pod_doc(name)
    if spec_top_level_key(d, "nodeSelector"):
        raise SystemExit(f"{name}: testPlacement must not set nodeSelector")
    if spec_top_level_key(d, "affinity"):
        raise SystemExit(f"{name}: testPlacement must not set affinity")
    keys = spec_toleration_keys(d)
    for want in ("cube.tencent.com/control", "cube.tencent.com/compute"):
        if want not in keys:
            raise SystemExit(f"{name}: missing toleration key {want} (have {sorted(keys)})")
    print(f"OK {name} (testPlacement)")


def check_compute_placement(name, require_control_taint=False):
    d = pod_doc(name)
    if not has_node_selector_label(d, "cube.tencent.com/cube-node"):
        raise SystemExit(f"{name}: expected computePlacement nodeSelector")
    if has_node_selector_label(d, "cube.tencent.com/cube-control"):
        raise SystemExit(f"{name}: computePlacement must not pin cube-control")
    keys = spec_toleration_keys(d)
    if "cube.tencent.com/compute" not in keys:
        raise SystemExit(
            f"{name}: missing compute taint toleration (have {sorted(keys)})"
        )
    if require_control_taint and "cube.tencent.com/control" not in keys:
        raise SystemExit(
            f"{name}: single-node computePlacement missing control taint "
            f"(have {sorted(keys)})"
        )
    print(f"OK {name} (computePlacement)")


def check_image(name, container, needle):
    d = pod_doc(name)
    m = re.search(rf"- name: {container}\s*\n\s+image: (\S+)", d)
    if not m:
        raise SystemExit(f"{name}: container {container} missing")
    if needle not in m.group(1):
        raise SystemExit(
            f"{name}: container {container} image {m.group(1)} "
            f"does not contain {needle}"
        )


def check_readonly_mounts(name):
    d = pod_doc(name)
    m = re.search(r"(?ms)^      volumeMounts:\n((?:        .+\n)*)", d)
    if not m:
        raise SystemExit(f"{name}: no volumeMounts")
    entries = [e for e in re.split(r"(?m)^        - name:", m.group(1)) if e.strip()]
    if not entries:
        raise SystemExit(f"{name}: empty volumeMounts")
    for entry in entries:
        if "readOnly: true" not in entry:
            raise SystemExit(f"{name}: volumeMount missing readOnly: true:\n{entry}")


def assert_absent(name):
    if pod_doc(name, required=False) is not None:
        raise SystemExit(f"{name} must be omitted")


def assert_no_test_pods():
    found = hook_test_pods()
    if found:
        raise SystemExit(
            "test pods must be omitted when helmTest.enabled=false: "
            + ", ".join(found)
        )


def check_rendered_test_placements(require_control_taint_for_runtime=False):
    names = hook_test_pods()
    if not names:
        raise SystemExit("no helm test pods rendered")
    for name in names:
        if name.endswith("node-runtime-test"):
            check_compute_placement(
                name, require_control_taint=require_control_taint_for_runtime
            )
        else:
            check_test_placement(name)


def check_concat_custom_taints(name):
    d = pod_doc(name)
    if spec_top_level_key(d, "nodeSelector"):
        raise SystemExit(f"{name}: testPlacement must not set nodeSelector")
    if spec_top_level_key(d, "affinity"):
        raise SystemExit(f"{name}: testPlacement must not set affinity")
    keys = spec_toleration_keys(d)
    for want in ("custom/control", "custom/compute"):
        if want not in keys:
            raise SystemExit(
                f"{name}: concat missing {want} (have {sorted(keys)})"
            )
    print(f"OK {name} (custom taint concat)")


mode = pathlib.Path(sys.argv[1]).name

if mode == "default.yaml":
    for name in TEST_PLACEMENT_PODS:
        check_test_placement(name)
    check_compute_placement(RUNTIME_TEST_POD)
    check_rendered_test_placements()

    check_image("helm-guard-cube-health-test", "curl", "curlimages/curl")
    check_image("helm-guard-cube-dns-test", "dns", "curlimages/curl")
    check_image("helm-guard-cube-node-runtime-test", "node-runtime", "busybox")
    check_image("helm-guard-cube-proxy-control-test", "proxy", "curlimages/curl")

    d = pod_doc("helm-guard-cube-proxy-control-test")
    if "/admin/healthz" not in d:
        raise SystemExit("proxy-control-test does not probe admin healthz")
    if "CUBE_PROXY_ADMIN_TOKEN" not in d or "secretKeyRef" not in d or "cube-admin-token" not in d:
        raise SystemExit("proxy-control-test does not source the admin token from the release Secret")
    if not uncommented_has(d, "X-Cube-Admin-Token"):
        raise SystemExit("proxy-control-test missing X-Cube-Admin-Token header")
    if not uncommented_has(d, "curl -4"):
        raise SystemExit("proxy-control-test must pass curl -4")
    if not uncommented_has(d, "--retry-connrefused"):
        raise SystemExit("proxy-control-test must pass --retry-connrefused")
    if uncommented_has(d, "--retry-all-errors"):
        raise SystemExit("proxy-control-test must not pass --retry-all-errors (retries HTTP 4xx)")
    if 'test "$status" = "200"' not in d:
        raise SystemExit("proxy-control-test must assert HTTP 200")

    d = pod_doc("helm-guard-cube-dns-test")
    if "getent ahostsv4" not in d:
        raise SystemExit("dns-test does not use getent ahostsv4")
    if uncommented_has(d, "nslookup"):
        raise SystemExit("dns-test still uses nslookup")
    if not uncommented_has(d, "tries=20"):
        raise SystemExit("dns-test must retry with tries=20")
    if "printf 'could not resolve %s" not in d:
        raise SystemExit("dns-test diagnostics must printf quoted domain names")

    d = pod_doc("helm-guard-cube-health-test")
    if "command curl -4" not in d:
        raise SystemExit("health-test must wrap curl with -4")

    d = pod_doc("helm-guard-cube-node-image-test")
    if "command curl -4" not in d:
        raise SystemExit("node-image-test must wrap curl with -4")

    check_readonly_mounts("helm-guard-cube-node-runtime-test")
    d = pod_doc("helm-guard-cube-node-runtime-test")
    if "runAsUser: 0" not in d or "runAsGroup: 0" not in d:
        raise SystemExit("node-runtime-test must pin runAsUser/runAsGroup 0")
    if "allowPrivilegeEscalation: false" not in d or 'drop: ["ALL"]' not in d:
        raise SystemExit("node-runtime-test securityContext missing privilege hardening")

    print("helm test default placement/image/probe guard passed")

elif mode == "control-only.yaml":
    check_rendered_test_placements()
    assert_absent(RUNTIME_TEST_POD)
    assert_absent("helm-guard-cube-node-image-test")
    print("helm test control-only placement guard passed")

elif mode == "compute-only.yaml":
    check_rendered_test_placements()
    assert_absent("helm-guard-cube-mysql-test")
    assert_absent("helm-guard-cube-redis-test")
    assert_absent("helm-guard-cube-proxy-control-test")
    assert_absent("helm-guard-cube-dns-test")
    print("helm test compute-only placement guard passed")

elif mode == "single-node.yaml":
    check_rendered_test_placements(require_control_taint_for_runtime=True)
    print("helm test single-node placement guard passed")

elif mode == "custom-taint.yaml":
    check_concat_custom_taints("helm-guard-cube-health-test")
    print("helm test custom taint concat guard passed")

elif mode == "disabled.yaml":
    assert_no_test_pods()
    print("helm test disabled omit guard passed")

else:
    raise SystemExit(f"unknown render {mode}")
PY
}

render "$TMP_DIR/default.yaml"
python_check "$TMP_DIR/default.yaml"

render "$TMP_DIR/control-only.yaml" --set cubeNode.enabled=false
python_check "$TMP_DIR/control-only.yaml"

render "$TMP_DIR/compute-only.yaml" \
  --set controlPlane.enabled=false \
  --set externalControlPlane.enabled=true \
  --set-string externalControlPlane.masterEndpoint=http://10.0.0.1:8080 \
  --set-string externalControlPlane.opsEndpoint=http://10.0.0.1:3010 \
  --set mysql.enabled=false \
  --set redis.enabled=false
python_check "$TMP_DIR/compute-only.yaml"

render "$TMP_DIR/single-node.yaml" -f "$CHART_DIR/values-single-node.yaml"
python_check "$TMP_DIR/single-node.yaml"

render "$TMP_DIR/custom-taint.yaml" \
  --set-json 'placement.controlPlane.tolerations=[{"key":"custom/control","operator":"Exists","effect":"NoSchedule"}]' \
  --set-json 'placement.compute.tolerations=[{"key":"custom/compute","operator":"Exists","effect":"NoSchedule"}]'
python_check "$TMP_DIR/custom-taint.yaml"

render "$TMP_DIR/disabled.yaml" --set helmTest.enabled=false
python_check "$TMP_DIR/disabled.yaml"
