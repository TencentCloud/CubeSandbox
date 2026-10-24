#!/bin/sh
# Exercise the real node-init loopback command without host mounts or root.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

python3 - "$SCRIPT_DIR/cube-node-init.sh" "$TMP_DIR" <<'PY'
import os
import pathlib
import shutil
import subprocess
import sys

script, tmp = map(pathlib.Path, sys.argv[1:])
bin_dir = tmp / "bin"
bin_dir.mkdir()
real_mkdir = shutil.which("mkdir")
real_truncate = shutil.which("truncate")

# Only host-namespace/kernel operations are stubbed. The command passed to
# nsenter still executes through /bin/sh, with real directory/file creation.
stub = bin_dir / "stub"
stub.write_text("#!/usr/bin/env python3\n" + r'''
import os
import pathlib
import subprocess
import sys

name = pathlib.Path(sys.argv[0]).name
if name == "lsmod":
    sys.exit(0)
if name in ("xfs_info", "mountpoint"):
    sys.exit(1)
if name == "nsenter":
    assert sys.argv[1:-1] == [
        "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid",
        "--", "/bin/sh", "-c",
    ], sys.argv
    # Never touch the test machine's fstab.
    command = sys.argv[-1].replace("/etc/fstab", os.environ["TEST_FSTAB"])
    sys.exit(subprocess.run(["/bin/sh", "-c", command]).returncode)
with open(os.environ["TEST_CALLS"], "a") as calls:
    calls.write(name + "\n")
if os.environ.get("FAIL_STEP") == name:
    sys.exit(42)
if name in ("mkdir", "truncate"):
    sys.exit(subprocess.run([os.environ["REAL_" + name.upper()], *sys.argv[1:]]).returncode)
assert name in ("mkfs.xfs", "mount"), name
''')
stub.chmod(0o755)
for command in ("lsmod", "xfs_info", "mountpoint", "nsenter", "mkdir",
                "truncate", "mkfs.xfs", "mount"):
    (bin_dir / command).symlink_to(stub)

for failure in ("", "quoted", "mkdir", "truncate", "mkfs.xfs", "mount", "disabled"):
    case = tmp / (failure or "success")
    case.mkdir()
    # The image's parent is separate from the custom Cubelet directory and
    # does not exist. A separate case also exercises host-shell path quoting.
    image = case / ("backing'images" if failure == "quoted" else "backing-images") / "cubelet-xfs.img"
    target = case / "custom-cubelet"
    fstab = case / "fstab"
    fstab.write_text("# existing entries\n")
    calls = case / "calls"
    env = os.environ.copy()
    env.update({
        "HOST_ROOT": str(case / "host"), "STATE_DIR": "/bootstrap",
        "PATH": str(bin_dir) + os.pathsep + env["PATH"],
        "CUBE_PVM_ENABLE": "0", "SKIP_IF_NODE_PREP_READY": "false",
        "DATA_CUBELET": str(target), "LOOPBACK_IMAGE_PATH": str(image),
        "LOOPBACK_ENABLED": "false" if failure == "disabled" else "true",
        "LOOPBACK_SIZE": "1K", "TEST_FSTAB": str(fstab),
        "TEST_CALLS": str(calls), "FAIL_STEP": failure,
        "REAL_MKDIR": real_mkdir, "REAL_TRUNCATE": real_truncate,
    })
    for flag in (
        "CREATE_HOST_DIRS", "REQUIRE_KVM", "REQUIRE_XFS", "CHMOD_KVM",
        "LOAD_KVM_MODULE", "WRITE_UDEV_RULE", "CHECK_MASTER_CONNECTIVITY",
        "CHECK_MEMORY", "CHECK_CGROUP_CPU", "CHECK_BPF_FS", "CHECK_GLIBC",
        "CHECK_CIDR", "CHECK_HOST_PORTS", "CHECK_CUBECOW_DEPS",
    ):
        env[flag] = "false"
    result = subprocess.run(["sh", str(script)], env=env, capture_output=True, text=True)
    if failure not in ("", "quoted", "disabled"):
        assert result.returncode != 0, (failure, result.stdout, result.stderr)
        assert fstab.read_text() == "# existing entries\n", failure
        recorded = calls.read_text().splitlines()
        assert recorded[-1] == failure, recorded
    elif failure == "disabled":
        assert result.returncode == 0, result.stderr
        assert not image.parent.exists()
        assert fstab.read_text() == "# existing entries\n"
    else:
        assert result.returncode == 0, (result.stdout, result.stderr)
        assert target.is_dir()
        assert image.stat().st_size == 1024
        entry = f"{image} {target} xfs loop,pquota 0 0\n"
        assert fstab.read_text() == "# existing entries\n" + entry
        subprocess.run(["sh", str(script)], env=env, check=True, capture_output=True)
        assert fstab.read_text().count(entry) == 1
        recorded = calls.read_text().splitlines()
        assert recorded.count("truncate") == recorded.count("mkfs.xfs") == 1
    print("ok: loopback " + (failure or "parent creation and idempotent retry"))
PY
