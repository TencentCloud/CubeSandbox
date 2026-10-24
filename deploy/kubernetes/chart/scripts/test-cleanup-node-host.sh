#!/bin/sh
# Verify cleanup argv with mocked mutating commands; never clean the test host.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

python3 - "$SCRIPT_DIR/cleanup-node-host.sh" "$TMP_DIR" <<'PY'
import json
import os
import pathlib
import subprocess
import sys

script, tmp = map(pathlib.Path, sys.argv[1:])
bin_dir = tmp / "bin"
bin_dir.mkdir()
stub = bin_dir / "stub"
stub.write_text("#!/usr/bin/env python3\n" + r'''
import json
import os
import pathlib
import sys

name = pathlib.Path(sys.argv[0]).name
if name == "id":
    print(os.environ.get("TEST_UID", "0"))
elif name == "ip":
    sys.exit(1)  # no residual interfaces
else:
    assert name in ("rm", "iptables"), name
    with open(os.environ["TEST_CALLS"], "a") as log:
        log.write(json.dumps([name, *sys.argv[1:]]) + "\n")
''')
stub.chmod(0o755)
for command in ("id", "ip", "rm", "iptables"):
    (bin_dir / command).symlink_to(stub)

defaults = {
    "TOOLBOX_ROOT": "/usr/local/services/cubetoolbox",
    "DATA_CUBELET": "/data/cubelet",
    "DATA_CUBE_SHIM": "/data/cube-shim",
    "DATA_CUBE_SHARED": "/data/cube-shared",
    "DATA_LOG": "/data/log",
    "DATA_SNAPSHOT_PACK": "/data/snapshot_pack",
    "LOOPBACK_IMAGE_PATH": "/data/cubelet-xfs.img",
    "TMP_CUBE": "/tmp/cube",
    "BOOTSTRAP_STATE": "/var/lib/cube-node-bootstrap",
}
calls_file = tmp / "calls"


def run(overrides):
    calls_file.write_text("")
    env = os.environ.copy()
    for name in (*defaults, "DRY_RUN", "DATA_SHARED", "TEST_UID"):
        env.pop(name, None)
    env.update({
        "PATH": str(bin_dir) + os.pathsep + env["PATH"],
        "TEST_CALLS": str(calls_file),
    })
    env.update(overrides)
    result = subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True)
    return result, [json.loads(line) for line in calls_file.read_text().splitlines()]


def expected_removals(paths):
    return [
        ["rm", "-rf", "--", paths["TOOLBOX_ROOT"]],
        ["rm", "-rf", "--", paths["BOOTSTRAP_STATE"]],
        ["rm", "-rf", "--", *[paths[k] for k in (
            "DATA_CUBELET", "DATA_CUBE_SHIM", "DATA_CUBE_SHARED", "TMP_CUBE")]],
        ["rm", "-rf", "--", paths["LOOPBACK_IMAGE_PATH"],
         *[paths["DATA_LOG"] + "/" + name for name in ("Cubelet", "CubeShim", "CubeVmm")],
         paths["DATA_SNAPSHOT_PACK"]],
    ]


custom = {name: "/relocated storage/operator's " + name.lower() for name in defaults}
custom["DATA_CUBELET"] += " $(printf should-not-run)"
for paths in (defaults, custom):
    result, calls = run({**paths, "DATA_SHARED": "/retained/user-data"})
    assert result.returncode == 0, (result.stdout, result.stderr)
    assert [c for c in calls if c[0] == "rm"] == expected_removals(paths), calls
    assert all("/retained/user-data" not in c and "/data/shared" not in c for c in calls)
    assert paths["DATA_LOG"] not in [arg for c in calls for arg in c], calls
print("ok: default and relocated targets; literal paths with spaces, quotes and shell syntax")
print("ok: shared user data and unrelated log children are not cleanup targets")

result, calls = run({**custom, "DRY_RUN": "1"})
assert result.returncode == 0 and not calls, (result, calls)
assert "DRY_RUN:" in result.stdout
print("ok: dry run invokes no mutating commands")

for name in defaults:
    for unsafe in ("", "/", "///", "relative/path", "/data/..", "/data/../elsewhere", "/data/."):
        result, calls = run({name: unsafe})
        assert result.returncode != 0 and not calls, (name, unsafe, result, calls)
        assert "unsafe cleanup path" in result.stderr, result.stderr
print("ok: every empty, root, relative or dot-segment target is rejected before cleanup")

result, calls = run({**custom, "DATA_CUBELET": custom["DATA_CUBELET"] + "///"})
assert result.returncode == 0, result.stderr
assert [c for c in calls if c[0] == "rm"] == expected_removals(custom)
print("ok: trailing slashes do not change removal targets")

result, calls = run({"TEST_UID": "1000"})
assert result.returncode != 0 and not calls
assert "must run as root" in result.stderr
print("ok: root requirement is preserved")
PY
