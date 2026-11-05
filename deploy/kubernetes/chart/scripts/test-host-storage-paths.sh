#!/bin/sh
# Exercise the host storage -> installer -> runtime path contract without a cluster.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

python3 - "$CHART_DIR" "$TMP_DIR" <<'PY'
import json
import os
import pathlib
import re
import subprocess
import sys

chart, tmp = map(pathlib.Path, sys.argv[1:])
scripts = chart.parent / "images/scripts"
paths = {
    "dataCubelet": ("data-cubelet", "/data/cubelet", "DATA_CUBELET"),
    "dataLog": ("data-log", "/data/log", "DATA_LOG"),
    "dataCubeShim": ("data-cube-shim", "/data/cube-shim", "DATA_CUBE_SHIM"),
    "dataSnapshotPack": ("data-snapshot-pack", "/data/snapshot_pack", "DATA_SNAPSHOT_PACK"),
    "dataCubeShared": ("data-cube-shared", "/data/cube-shared", "DATA_CUBE_SHARED"),
    "dataShared": ("data-shared", "/data/shared", "DATA_SHARED"),
    "tmpCube": ("tmp-cube", "/tmp/cube", "TMP_CUBE"),
}


def named_block(text, name):
    """Read one named list entry from Helm's rendered manifests."""
    match = re.search(r"(?m)^( +)- name: " + re.escape(name) + r"\n", text)
    assert match, f"missing entry: {name}"
    indent = len(match[1])
    lines = []
    for line in text[match.end():].splitlines():
        if line.strip() and len(line) - len(line.lstrip()) <= indent:
            break
        lines.append(line)
    return "\n".join(lines)


def value(text, key):
    match = re.search(r"(?m)^ +" + re.escape(key) + r": (.+)$", text)
    assert match, f"missing field: {key}"
    raw = match[1]
    return json.loads(raw) if raw.startswith('"') else raw


def host_path(pod, container, volume, container_path):
    mount = pathlib.PurePosixPath(value(named_block(container, volume), "mountPath"))
    relative = pathlib.PurePosixPath(container_path).relative_to(mount)
    volumes = re.split(r"(?m)^ +volumes:\n", pod)[-1]
    return pathlib.PurePosixPath(value(named_block(volumes, volume), "path")) / relative


# Only kernel probes are stubbed; node-init creates real directories in a fake host root.
bin_dir = tmp / "bin"
bin_dir.mkdir()
for command in ("lsmod", "xfs_info"):
    stub = bin_dir / command
    stub.write_text("#!/bin/sh\nexit 0\n")
    stub.chmod(0o755)

entrypoint_helpers = tmp / "component-helpers.sh"
entrypoint_helpers.write_text("\n".join(
    line for line in (scripts / "component-entrypoint.sh").read_text().splitlines()
    if line != 'main "$@"'
) + "\n")

for custom in (False, True):
    case = "custom" if custom else "default"
    configured = {
        key: f"/var/lib/cubesandbox/{volume}" if custom else target
        for key, (volume, target, _) in paths.items()
    }
    args = [
        "helm", "template", "storage-paths", str(chart),
        "--set-string", "mysql.password=test",
        "--set-string", "mysql.rootPassword=test",
        "--set-string", "redis.password=test",
        "--set", "cubeS3lvol.enabled=true",
    ]
    if custom:
        for key, target in configured.items():
            args += ["--set-string", f"hostPaths.{key}={target}"]
        args += ["--set-string", "hostPaths.bootstrapState=/var/lib/cubesandbox/bootstrap"]
    rendered = subprocess.check_output(args, text=True)
    documents = rendered.split("\n---\n")

    def component(name):
        matches = [d for d in documents if re.search(r"(?m)^kind: DaemonSet$", d)
                   and f"\n    app.kubernetes.io/component: {name}\n" in d]
        assert len(matches) == 1, (name, len(matches))
        return matches[0]

    node = component("cube-node")
    installer = component("cube-node-installer")
    bootstrap = component("cube-node-bootstrap")
    cubelet = named_block(node, "cubelet")
    node_init = named_block(bootstrap, "cube-node-init")

    for key, (volume, target, env_name) in paths.items():
        actual = value(named_block(cubelet, volume), "mountPath")
        assert actual == target, f"{case}: Cubelet needs {target}, mounted at {actual}"
        assert str(host_path(node, cubelet, volume, target)) == configured[key]
        # node-init runs host mount commands and probes XFS at the host-side path.
        assert value(named_block(node_init, volume), "mountPath") == configured[key]
        assert str(host_path(bootstrap, node_init, volume, configured[key])) == configured[key]
        if custom or env_name == "DATA_CUBELET":
            assert value(named_block(node_init, env_name), "value") == configured[key]

    for container_name, volumes in [
        ("cube-egress", ["dataLog"]),
        ("cube-s3lvol", ["dataCubelet", "dataLog"]),
    ]:
        container = named_block(node, container_name)
        for key in volumes:
            volume, target, _ = paths[key]
            assert str(host_path(node, container, volume, target)) == configured[key]

    health = next(d for d in documents if re.search(r"(?m)^  name: .+-node-runtime-test$", d))
    health_container = named_block(health, "node-runtime")
    for key in ("dataCubelet", "tmpCube"):
        volume, target, _ = paths[key]
        assert str(host_path(health, health_container, volume, target)) == configured[key]

    version_root = "/data/cubelet/root/component_versions"
    for name in ("cube-shim", "cube-kernel", "cube-guest", "cube-agent"):
        container = named_block(installer, name + "-install")
        root = value(named_block(container, "COMPONENT_VERSIONS_ROOT"), "value")
        assert root == version_root, (case, name, root)
        assert host_path(installer, container, "data-cubelet", root) == host_path(
            node, cubelet, "data-cubelet", version_root
        )

    host = tmp / case
    env = os.environ.copy()
    for match in re.finditer(r'(?m)^ +- name: (\w+)\n +value: (.+)$', node_init):
        env[match[1]] = json.loads(match[2]) if match[2].startswith('"') else match[2]
    env.update({
        "HOST_ROOT": str(host),
        "PATH": str(bin_dir) + os.pathsep + env["PATH"],
        "STATE_DIR": "/var/lib/cubesandbox/bootstrap",
        "CUBE_PVM_ENABLE": "0", "SKIP_IF_NODE_PREP_READY": "false",
        "CREATE_HOST_DIRS": "true", "LOOPBACK_ENABLED": "false",
    })
    for flag in (
        "REQUIRE_KVM", "REQUIRE_XFS", "CHMOD_KVM", "LOAD_KVM_MODULE", "WRITE_UDEV_RULE",
        "CHECK_MASTER_CONNECTIVITY", "CHECK_MEMORY", "CHECK_CGROUP_CPU", "CHECK_BPF_FS",
        "CHECK_GLIBC", "CHECK_CIDR", "CHECK_HOST_PORTS", "CHECK_CUBECOW_DEPS",
    ):
        env[flag] = "false"
    subprocess.run(["sh", str(scripts / "cube-node-init.sh")], env=env, check=True)
    for target in configured.values():
        assert (host / target.lstrip("/")).is_dir(), f"missing host directory: {target}"
    assert (host / configured["dataCubeShared"].lstrip("/") / "volume").is_dir()
    if custom:
        assert not (host / "data").exists(), "bootstrap wrote to default host storage"

    # Stage a real versioned artifact with the production installer, then read it
    # through Cubelet's fixed runtime path and the rendered hostPath mapping.
    source = tmp / (case + "-shim-image")
    (source / "bin").mkdir(parents=True)
    (source / "version").write_text("v-path-test\n")
    (source / "bin/cube-runtime").write_text("runtime artifact\n")
    shim = named_block(installer, "cube-shim-install")
    staged_root = host / str(host_path(installer, shim, "data-cubelet", version_root)).lstrip("/")
    subprocess.run([
        "bash", "-c",
        'source "$1"; '
        'CUBE_COMPONENT=cube-shim; COMPONENT_VERSIONS_ROOT="$3"; '
        'inventory_component_version "$2" cube-shim',
        "inventory-test", str(entrypoint_helpers), str(source), str(staged_root),
    ], check=True)
    runtime_path = version_root + "/cube-shim/v-path-test/bin/cube-runtime"
    visible = host / str(host_path(node, cubelet, "data-cubelet", runtime_path)).lstrip("/")
    assert visible.read_text() == "runtime artifact\n"
    print(f"ok: {case} host storage, bootstrap, installer and runtime paths")
PY
