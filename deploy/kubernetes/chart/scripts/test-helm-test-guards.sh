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

    def doc_named(kind, name):
        for candidate in docs:
            if re.search(rf"^kind: {kind}$", candidate, re.M) and re.search(
                rf"^  name: {re.escape(name)}$", candidate, re.M
            ):
                return candidate
        raise SystemExit(f"{kind} {name} not found")

    # A second randAlphaNum render would diverge from the release Secret on a
    # fresh install, so every resume purge would receive 403 from CubeProxy.
    secret_doc = doc_named("Secret", "helm-guard-cube-secret")
    if not re.search(r'^  cube-admin-token: ".+"$', secret_doc, re.M):
        raise SystemExit("release Secret has no cube-admin-token")
    master_conf = doc_named("Secret", "helm-guard-cube-master-config")
    admin_token = re.search(r'^\s+admin_token: "(.*)"$', master_conf, re.M)
    if admin_token is None or admin_token.group(1) != "":
        raise SystemExit("master config must not render its own admin token")
    admin_env = (
        r"(?ms)^            - name: CUBE_PROXY_ADMIN_TOKEN\n"
        r"              valueFrom:\n"
        r"                secretKeyRef:\n"
        r"                  name: helm-guard-cube-secret\n"
        r"                  key: cube-admin-token$"
    )
    for deployment in ("helm-guard-cube-master", "helm-guard-cube-proxy"):
        if not re.search(admin_env, doc_named("Deployment", deployment)):
            raise SystemExit(
                f"{deployment} does not read the admin token from the release Secret"
            )
    print("master and proxy share one admin token source")

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
    if "mountPath: /dev/shm" not in d or "name: dev-shm" not in d:
        raise SystemExit("node-runtime-test must mount host /dev/shm read-only")
    if "test -d /dev/shm/cubelet/network-runtime/state" not in d:
        raise SystemExit("node-runtime-test must check the network runtime state dir")

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

# The admin token reaches every consumer as an env secretKeyRef, which a running
# container resolves only at start. Rotating it must therefore move all three
# pod templates: otherwise the new value is silently unused, or the one pod that
# restarts first validates a token its peers no longer accept.
render "$TMP_DIR/admin-token-a.yaml" --set-string lifecycleManager.adminToken=guard-rotation-token-aaa
render "$TMP_DIR/admin-token-b.yaml" --set-string lifecycleManager.adminToken=guard-rotation-token-bbb
for f in "$TMP_DIR/admin-token-a.yaml" "$TMP_DIR/admin-token-b.yaml"; do
  count="$(grep -c 'checksum/admin-token:' "$f" || true)"
  if [ "$count" -ne 3 ]; then
    echo "checksum/admin-token must be on master, proxy and lifecycle-manager (got $count)" >&2
    exit 1
  fi
  distinct="$(grep 'checksum/admin-token:' "$f" | awk '{print $2}' | sort -u | wc -l | tr -d ' ')"
  if [ "$distinct" -ne 1 ]; then
    echo "checksum/admin-token differs across the three workloads" >&2
    exit 1
  fi
done
# The count and the shared value above would still pass if one workload carried
# two annotations and another none, so check where each annotation landed.
for f in "$TMP_DIR/admin-token-a.yaml" "$TMP_DIR/admin-token-b.yaml"; do
  python3 - "$f" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
carriers = []
for doc in text.split("\n---\n"):
    if "checksum/admin-token:" not in doc:
        continue
    if re.search(r"^kind: Deployment$", doc, re.M) is None:
        sys.exit(f"checksum/admin-token sits outside a Deployment: {sys.argv[1]}")
    if doc.count("checksum/admin-token:") != 1:
        sys.exit(f"checksum/admin-token appears more than once on one workload: {sys.argv[1]}")
    name = re.search(r"^  name: (.+)$", doc, re.M)
    carriers.append(name.group(1).strip() if name else "<unnamed>")
if len(carriers) != 3 or len(set(carriers)) != 3:
    sys.exit(f"checksum/admin-token must be on master, proxy and lifecycle-manager, got {carriers}")
PY
done
sum_a="$(grep -m1 'checksum/admin-token:' "$TMP_DIR/admin-token-a.yaml" | awk '{print $2}')"
sum_b="$(grep -m1 'checksum/admin-token:' "$TMP_DIR/admin-token-b.yaml" | awk '{print $2}')"
if [ "$sum_a" = "$sum_b" ]; then
  echo "checksum/admin-token did not change with lifecycleManager.adminToken" >&2
  exit 1
fi
echo "admin token rotation rolls master, proxy and lifecycle-manager"
