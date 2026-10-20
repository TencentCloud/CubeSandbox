#!/bin/sh
# Cloud-free regression tests: rendered env and real readiness scripts with a
# fake host root and mocked kernel/XFS probes. This is not a mount or cluster E2E.
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
import shlex
import shutil
import subprocess
import sys
import textwrap

chart, tmp = map(pathlib.Path, sys.argv[1:])
scripts = chart.parent / "images/scripts"
library = scripts / "node-prep-lib.sh"
kernel = "6.1.0-storage-test"
paths = {
    "DATA_CUBELET": ("hostPaths.dataCubelet", "/data/cubelet"),
    "DATA_LOG": ("hostPaths.dataLog", "/data/log"),
    "DATA_CUBE_SHIM": ("hostPaths.dataCubeShim", "/data/cube-shim"),
    "DATA_SNAPSHOT_PACK": ("hostPaths.dataSnapshotPack", "/data/snapshot_pack"),
    "DATA_CUBE_SHARED": ("hostPaths.dataCubeShared", "/data/cube-shared"),
    "DATA_SHARED": ("hostPaths.dataShared", "/data/shared"),
    "TMP_CUBE": ("hostPaths.tmpCube", "/tmp/cube"),
    "LOOPBACK_IMAGE_PATH": (
        "bootstrap.nodeInit.dataCubelet.loopback.imagePath", "/data/cubelet-xfs.img"
    ),
}
roles = ("invalidate-stale-node-prep", "cube-node-init",
         "write-node-prep-ready", "wait-node-prep")


def named_block(text, name):
    """Read one named list entry without adding a YAML dependency."""
    matches = list(re.finditer(r"(?m)^( +)- name: " + re.escape(name) + r"\n", text))
    assert len(matches) == 1, (name, len(matches))
    match = matches[0]
    indent = len(match[1])
    lines = []
    for line in text[match.end():].splitlines():
        if line.strip() and len(line) - len(line.lstrip()) <= indent:
            break
        lines.append(line)
    return "\n".join(lines)


def rendered_case(prefix=None):
    args = ["helm", "template", "node-prep-storage", str(chart),
            "--set-string", "mysql.password=test",
            "--set-string", "mysql.rootPassword=test",
            "--set-string", "redis.password=test"]
    expected = {name: default for name, (_, default) in paths.items()}
    if prefix:
        expected = {name: f"/{prefix}/{pathlib.PurePosixPath(default).name}"
                    for name, (_, default) in paths.items()}
        for name, (key, _) in paths.items():
            args += ["--set-string", f"{key}={expected[name]}"]
    documents = subprocess.check_output(args, text=True).split("\n---\n")

    def component(name):
        matches = [d for d in documents if re.search(r"(?m)^kind: DaemonSet$", d)
                   and f"\n    app.kubernetes.io/component: {name}\n" in d]
        assert len(matches) == 1, (name, len(matches))
        return matches[0]

    bootstrap = component("cube-node-bootstrap")
    node = component("cube-node")
    containers = {name: named_block(node if name == "wait-node-prep" else bootstrap, name)
                  for name in roles}
    envs = {}
    for name, container in containers.items():
        entries = re.findall(r"(?m)^ +- name: (\w+)\n +value: (.+)$", container)
        envs[name] = {key: json.loads(value) if value.startswith('"') else value
                      for key, value in entries}
        for key in paths:
            assert len(re.findall(r"(?m)^ +- name: " + re.escape(key) + r"$",
                                  container)) == 1, (prefix, name, key)
            assert sum(entry[0] == key for entry in entries) == 1, (prefix, name, key)
        actual = {key: envs[name][key] for key in paths}
        assert actual == expected, (prefix, name, actual, expected)
    # The invalidator must run before PVM waits or node-init, including upgrades.
    assert bootstrap.index("- name: invalidate-stale-node-prep") < bootstrap.index(
        "- name: wait-pvm-host") < bootstrap.index("- name: cube-node-init")
    invalidator = containers["invalidate-stale-node-prep"]
    command = re.search(r"(?m)^ +command: (.+)$", invalidator)
    assert command and json.loads(command[1]) == ["/bin/sh", "-ec"]
    match = re.search(r"(?ms)^ +args:\n +\- \|\n(.*?)^ +env:\n", invalidator)
    assert match, "missing rendered invalidator command"
    command = textwrap.dedent(match[1])
    assert command.count("/scripts/node-prep-lib.sh") == 1
    command = command.replace("/scripts/node-prep-lib.sh", shlex.quote(str(library)))
    # PVM and generation stay host-derived, outside the frozen Big Pod env.
    for key in ("PVM_ENABLED", "NODE_INIT_ENABLED", "PREP_GENERATION",
                "DESIRED_KERNEL_PATTERN", "KERNEL_BOOT_ARGS"):
        assert key not in envs["wait-node-prep"], key
    print(f"ok: {prefix or 'default'} render has the same eight storage env keys once in all four roles")
    return envs, command


rendered_case()
new, new_invalidator = rendered_case("new")
moved, moved_invalidator = rendered_case("moved")

# Real scripts write only beneath this temporary fake host. Kernel/XFS probes
# are mocked; any attempted namespace entry, mount or host mutation fails closed.
bin_dir = tmp / "bin"
bin_dir.mkdir()


def stub(name, body):
    path = bin_dir / name
    path.write_text("#!/bin/sh\nset -eu\n" + body + "\n")
    path.chmod(0o755)


stub("uname", f'printf "%s\\n" "{kernel}"')
stub("lsmod", 'if [ "${TEST_PVM_MODULE:-0}" = 1 ]; then printf "kvm_pvm 1 0\\n"; fi')
stub("xfs_info", 'printf "%s\\n" "$*" >> "$XFS_LOG"\n[ "${TEST_XFS_OK:-1}" = 1 ]')
real_sleep = shlex.quote(shutil.which("sleep"))
stub("sleep", f'[ "$1" != infinity ] || exit 0\nexec {real_sleep} "$@"')
for command in ("nsenter", "chroot", "mount", "umount", "modprobe",
                "mkfs.xfs", "losetup", "truncate", "sysctl", "kubectl"):
    stub(command, 'printf "unexpected host operation: %s\\n" "$0 $*" >> "$HOST_OPS_LOG"\nexit 99')

disabled = (
    "REQUIRE_KVM", "CHMOD_KVM", "LOAD_KVM_MODULE", "WRITE_UDEV_RULE",
    "CHECK_MASTER_CONNECTIVITY", "CHECK_MEMORY", "CHECK_CGROUP_CPU", "CHECK_BPF_FS",
    "CHECK_GLIBC", "CHECK_CIDR", "CHECK_HOST_PORTS", "CHECK_CUBECOW_DEPS",
)


def environment(rendered, role, host, **overrides):
    # Do not let caller-exported bootstrap variables affect these fixtures.
    env = {"PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "LANG": "C"}
    env.update(rendered[role])
    env.update({
        "HOST_ROOT": str(host), "PVM_ENABLED": "0", "CUBE_PVM_ENABLE": "0",
        "KERNEL_BOOT_ARGS": "", "DESIRED_KERNEL_PATTERN": "",
        "WAIT_TIMEOUT_SECONDS": "0", "WAIT_POLL_SECONDS": "0.05",
        "WAIT_READY_MARKER": str(host / "wait-node-prep.ready"),
        "XFS_LOG": str(host / "xfs.log"), "HOST_OPS_LOG": str(tmp / "host-operations.log"),
        "SKIP_IF_NODE_PREP_READY": "true", "CREATE_HOST_DIRS": "true",
        "LOOPBACK_ENABLED": "false", "REQUIRE_XFS": "true",
    })
    env.update({flag: "false" for flag in disabled})
    env.update(overrides)
    host.mkdir(parents=True, exist_ok=True)
    return env


def run(args, env, success=True):
    result = subprocess.run(args, env=env, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=10)
    assert (result.returncode == 0) == success, (args, result.returncode, result.stdout)
    return result.stdout


def helper(name, env, success=True):
    return run(["sh", "-ec", '. "$1"; ' + name, "node-prep-test", str(library)],
               env, success)


def invalidate(command, env):
    return run(["/bin/sh", "-ec", command], env)


def writer(env, success=True):
    return run(["sh", str(scripts / "write-node-prep-ready.sh")], env, success)


def node_init(env, success=True):
    return run(["sh", str(scripts / "cube-node-init.sh")], env, success)


def waiter(env, success=True):
    # A rejected sentinel must remove even an old successful waiter's marker.
    marker = pathlib.Path(env["WAIT_READY_MARKER"])
    marker.touch()
    output = run(["sh", str(scripts / "wait-node-prep.sh")], env, success)
    assert marker.exists() == success, output
    assert ("fingerprint matched" if success else "timeout") in output, output
    return output


def ready_path(env, name="node-prep-ready"):
    return pathlib.Path(env["HOST_ROOT"]) / env["STATE_DIR"].lstrip("/") / name


def assert_directories(env):
    for key in paths:
        if key != "LOOPBACK_IMAGE_PATH":
            assert (pathlib.Path(env["HOST_ROOT"]) / env[key].lstrip("/")).is_dir(), key
    assert (pathlib.Path(env["HOST_ROOT"]) /
            env["DATA_CUBE_SHARED"].lstrip("/") / "volume").is_dir()


host = tmp / "individual-paths"
write_env = environment(new, "write-node-prep-ready", host)
init_env = environment(new, "cube-node-init", host)
ready = ready_path(write_env)
writer(write_env)
original = ready.read_text()
assert original.startswith("version=2\n")
assert sum(line.startswith("storage_") for line in original.splitlines()) == 8
helper("node_prep_fingerprint_matches_file", init_env)
helper("node_prep_storage_fingerprint_matches_file", init_env)

for key in paths:
    ready.write_text(original)
    changed = {key: moved["cube-node-init"][key]}
    changed_init = environment(new, "cube-node-init", host, **changed)
    helper("node_prep_fingerprint_matches_file", changed_init, success=False)
    helper("node_prep_storage_fingerprint_matches_file", changed_init, success=False)
    waiter(environment(new, "wait-node-prep", host, **changed), success=False)
    invalidator_env = environment(new, "invalidate-stale-node-prep", host, **changed)
    invalidate(new_invalidator, invalidator_env)
    assert not ready.exists(), f"{key}: early invalidator kept stale sentinel"
    invalidate(new_invalidator, invalidator_env)  # An absent marker is idempotent.
    print(f"ok: changing only {key} rejects both fingerprints and waiter, then invalidates early")

# Preserve existing invalidation on preparation generation and node-init policy.
for changed in ({"PREP_GENERATION": "next"}, {"NODE_INIT_ENABLED": "0"}):
    ready.write_text(original)
    invalidate(new_invalidator, environment(new, "invalidate-stale-node-prep", host, **changed))
    assert not ready.exists(), changed

ready.write_text(original)
before = (ready.read_bytes(), ready.stat().st_mtime_ns, ready.stat().st_ino)
for _ in range(2):
    invalidate(new_invalidator, environment(new, "invalidate-stale-node-prep", host,
                                           PVM_ENABLED="1", DESIRED_KERNEL_PATTERN="irrelevant"))
assert before == (ready.read_bytes(), ready.stat().st_mtime_ns, ready.stat().st_ino)
output = node_init(dict(init_env, TEST_XFS_OK="0"))
assert "skipping node-init" in output and not (host / "xfs.log").exists(), output
waiter(environment(new, "wait-node-prep", host, PVM_ENABLED="1", PREP_GENERATION="irrelevant",
                   NODE_INIT_ENABLED="0", KERNEL_BOOT_ARGS="irrelevant",
                   DESIRED_KERNEL_PATTERN="does-not-match"))
print("ok: unchanged readiness is idempotent and unrelated waiter policy cannot override the host")

# Reproduce /new/* -> /moved/* with SKIP_IF_NODE_PREP_READY=true. The actual
# node-init must reach the failed XFS check, rather than trusting old readiness.
host = tmp / "storage-move"
new_init = environment(new, "cube-node-init", host)
node_init(new_init)
assert_directories(new_init)
writer(environment(new, "write-node-prep-ready", host))
moving_init = environment(moved, "cube-node-init", host, TEST_XFS_OK="0")
output = node_init(moving_init, success=False)
assert f'{moving_init["DATA_CUBELET"]} is not an XFS mount' in output, output
assert "skipping node-init" not in output
assert_directories(moving_init)
assert (host / "xfs.log").read_text().splitlines()[-2:] == [moving_init["DATA_CUBELET"]] * 2
waiter(environment(moved, "wait-node-prep", host), success=False)
invalidate(moved_invalidator, environment(moved, "invalidate-stale-node-prep", host))
assert not ready_path(moving_init).exists()
moving_init["TEST_XFS_OK"] = "1"
assert "cube node init completed" in node_init(moving_init)
writer(environment(moved, "write-node-prep-ready", host))
waiter(environment(moved, "wait-node-prep", host))
probe_log = (host / "xfs.log").read_text()
assert "skipping node-init" in node_init(dict(moving_init, TEST_XFS_OK="0"))
assert (host / "xfs.log").read_text() == probe_log
assert_directories(new_init)  # Changing readiness never migrates or deletes data.
print("ok: moved paths run real directory preparation and mocked XFS validation before fresh readiness")

# A version-1 marker cannot prove storage readiness. Start the real waiter
# first, then follow the rendered bootstrap order; it must accept the writer's
# replacement without a restart or an init/waiter deadlock.
host = tmp / "version-one-migration"
write_env = environment(new, "write-node-prep-ready", host)
writer(write_env)
ready = ready_path(write_env)
version_one = "\n".join(
    "version=1" if line == "version=2" else line
    for line in ready.read_text().splitlines() if not line.startswith("storage_")
) + "\n"
ready.write_text(version_one)
helper("node_prep_fingerprint_matches_file", environment(new, "cube-node-init", host), success=False)
wait_env = environment(new, "wait-node-prep", host)
waiter(wait_env, success=False)
wait_env["WAIT_TIMEOUT_SECONDS"] = "5"
process = subprocess.Popen(["sh", str(scripts / "wait-node-prep.sh")], env=wait_env,
                           text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
try:
    first_line = process.stdout.readline()
    assert "waiting for" in first_line and process.poll() is None, first_line
    invalidate(new_invalidator, environment(new, "invalidate-stale-node-prep", host))
    assert not ready.exists()
    migration_init = environment(new, "cube-node-init", host)
    run(["sh", str(scripts / "wait-pvm-host.sh")],
        dict(migration_init, PVM_FEATURE_ENABLED="0", CUBE_PVM_ENABLE="0"))
    assert ready_path(migration_init, "effective-pvm").read_text() == "0\n"
    assert "cube node init completed" in node_init(migration_init)
    assert_directories(migration_init)
    writer(write_env)
    output, _ = process.communicate(timeout=7)
    assert process.returncode == 0 and "fingerprint matched" in output, output
    assert pathlib.Path(wait_env["WAIT_READY_MARKER"]).exists()
finally:
    if process.poll() is None:
        process.kill()
        process.communicate()
assert ready.read_text().startswith("version=2\n")
assert sum(line.startswith("storage_") for line in ready.read_text().splitlines()) == 8
print("ok: version-1 migration invalidates, resolves PVM, prepares, writes version 2 and releases a running waiter")

# Storage validation must not weaken the live-kernel/PVM gates. PVM policy is
# read from host sentinels, even when the waiter's unrelated env disagrees.
host = tmp / "pvm-policy"
pvm_env = environment(new, "write-node-prep-ready", host, PVM_ENABLED="1",
                      DESIRED_KERNEL_PATTERN="^6[.]1[.]0-storage-test$")
helper("write_pvm_host_ready", pvm_env)
pvm_ready = ready_path(pvm_env, "pvm-host-ready")
assert pvm_ready.read_text().startswith("version=1\n"), "PVM host format must remain version 1"
writer(pvm_env)
ready = ready_path(pvm_env)
good_node, good_pvm = ready.read_text(), pvm_ready.read_text()
wait_env = environment(new, "wait-node-prep", host, PVM_ENABLED="0", PREP_GENERATION="other",
                       NODE_INIT_ENABLED="0", DESIRED_KERNEL_PATTERN="wrong-env-pattern",
                       KERNEL_BOOT_ARGS="wrong-env-boot-arg")
waiter(wait_env)
helper("node_prep_fingerprint_matches_file", pvm_env)

mutating = ready_path(pvm_env, "pvm-mutating")
mutating.touch()
waiter(wait_env, success=False)
helper("node_prep_fingerprint_matches_file", pvm_env, success=False)
assert "pvm-mutating" in writer(pvm_env, success=False)
mutating.unlink()

ready.write_text(good_node.replace(f"kernel={kernel}\n", "kernel=old-kernel\n"))
waiter(wait_env, success=False)
helper("node_prep_fingerprint_matches_file", pvm_env, success=False)
ready.write_text(good_node)

pvm_ready.unlink()
waiter(wait_env, success=False)
assert "fingerprint mismatch" in writer(pvm_env, success=False)
for bad_pvm in (
    good_pvm.replace(f"kernel={kernel}\n", "kernel=old-kernel\n"),
    good_pvm.replace("desired_pattern=^6[.]1[.]0-storage-test$", "desired_pattern=^wrong-kernel$"),
    good_pvm.replace("version=1\n", "version=2\n"),
):
    pvm_ready.write_text(bad_pvm)
    waiter(wait_env, success=False)
    helper("node_prep_fingerprint_matches_file", pvm_env, success=False)

missing_arg = "node-prep-storage-test-required-boot-arg=absent"
cmdline_path = pathlib.Path("/proc/cmdline")
assert missing_arg not in (cmdline_path.read_text() if cmdline_path.exists() else "")
ready.write_text(good_node.replace("boot_args=\n", f"boot_args={missing_arg}\n"))
pvm_ready.write_text(good_pvm.replace("boot_args=\n", f"boot_args={missing_arg}\n"))
waiter(wait_env, success=False)
ready.write_text(good_node)
pvm_ready.write_text(good_pvm)
waiter(wait_env)
print("ok: wrong kernel, missing/invalid PVM readiness, mutation and missing boot args stay rejected")

assert not (tmp / "host-operations.log").exists(), "test attempted a real host operation"
print("PASS: node-prep storage regression (mocked probes, no mounts or cluster)")
PY
