# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Host-mount volumes — share a host directory into the sandbox.

Tests:
  - metadata["hostdir-mount"] with readOnly=False  → read + write inside sandbox
  - metadata["hostdir-mount"] with readOnly=True   → read succeeds, write fails
  - write-back: files written inside sandbox appear on Cubelet host after destroy

Usage:
    export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
    python examples/volume.py

Note: hostPath is a directory on the **Cubelet node** (<YOUR_NODE_IP>), NOT on this
machine. Write-back (sandbox writes → host) is flushed after the sandbox is destroyed
(overlay merged on teardown), not in real-time.
"""
import sys
import os
import json
import subprocess

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

failures: list[str] = []


def check(tag: str, condition: bool, detail: str = "") -> None:
    if condition:
        print(f"  ✅ {tag}")
    else:
        msg = f"{tag}: {detail}" if detail else tag
        print(f"  ❌ {msg}")
        failures.append(msg)


CUBELET_HOST = os.environ.get("CUBE_PROXY_NODE_IP")
CUBELET_PORT = os.environ.get("CUBE_CUBELET_SSH_PORT", "22")
CUBELET_USER = os.environ.get("CUBE_CUBELET_SSH_USER")

if not CUBELET_HOST or not CUBELET_USER:
    print("ERROR: CUBE_PROXY_NODE_IP and CUBE_CUBELET_SSH_USER must be set")
    sys.exit(1)
HOST_DIR_RW = "/tmp/cube_volume_rw"
HOST_DIR_RO = "/tmp/cube_volume_ro"
MOUNT_PATH = "/mnt/data"


def ssh(cmd: str) -> tuple[str, int]:
    """Run a command on the Cubelet node via SSH."""
    r = subprocess.run(
        ["ssh", "-p", CUBELET_PORT, "-o", "StrictHostKeyChecking=no",
         f"{CUBELET_USER}@{CUBELET_HOST}", cmd],
        capture_output=True, text=True,
    )
    return (r.stdout.strip() + r.stderr.strip()), r.returncode


# ── 0. Prepare host directories on Cubelet node ──────────────────────────────
print("=== Preparing host directories on Cubelet node ===")
out, rc = ssh(f"mkdir -p {HOST_DIR_RW} {HOST_DIR_RO} && "
              f"echo 'Hello from host!' > {HOST_DIR_RW}/hello.txt && "
              f"echo 'Readonly file' > {HOST_DIR_RO}/readonly.txt && "
              f"echo 'ok'")
print(f"  ssh setup: {out!r} (rc={rc})")
check("host dir setup", rc == 0, out)

# ── 1. readOnly=False: read + write ─────────────────────────────────────────
print("\n=== readOnly=False (read + write) ===")
mounts_rw = json.dumps([
    {"hostPath": HOST_DIR_RW, "mountPath": MOUNT_PATH, "readOnly": False}
])
with Sandbox.create(metadata={"hostdir-mount": mounts_rw}) as sb:
    print(f"  Created: {sb}")

    # Read pre-existing file from host
    result = sb.run_code(f"open('{MOUNT_PATH}/hello.txt').read().strip()")
    print(f"  read hello.txt = {result.text!r}")
    check("rw: read host file", result.text == "Hello from host!", f"got {result.text!r}")

    # Write new file through the mount
    sb.run_code(f"open('{MOUNT_PATH}/from_sandbox.txt', 'w').write('Hi from sandbox!')")

    # List mount dir
    result = sb.run_code(f"import os; sorted(os.listdir('{MOUNT_PATH}'))")
    print(f"  ls {MOUNT_PATH} = {result.text}")
    check("rw: from_sandbox.txt in listing",
          result.text is not None and "from_sandbox.txt" in result.text,
          f"got {result.text!r}")

# Sandbox destroyed — overlay merged, check write-back
out, rc = ssh(f"cat {HOST_DIR_RW}/from_sandbox.txt")
print(f"  write-back on host: {out!r}")
check("rw: write-back to host after destroy", out == "Hi from sandbox!", f"got {out!r}")

# ── 2. readOnly=True: read succeeds, write fails ─────────────────────────────
print("\n=== readOnly=True ===")
mounts_ro = json.dumps([
    {"hostPath": HOST_DIR_RO, "mountPath": MOUNT_PATH, "readOnly": True}
])
with Sandbox.create(metadata={"hostdir-mount": mounts_ro}) as sb:
    print(f"  Created: {sb}")

    # Read should succeed
    result = sb.run_code(f"open('{MOUNT_PATH}/readonly.txt').read().strip()")
    print(f"  read readonly.txt = {result.text!r}")
    check("ro: read succeeds", result.text == "Readonly file", f"got {result.text!r}")

    # Write should fail (OSError / PermissionError)
    result = sb.run_code(f"open('{MOUNT_PATH}/should_fail.txt', 'w').write('x')")
    print(f"  write attempt error: {result.error}")
    check("ro: write raises error", result.error is not None,
          f"expected error but got result.text={result.text!r}")

print("\nAll sandboxes destroyed.")

# ── summary ──────────────────────────────────────────────────────────────────
print("\n" + "=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
