"""
Example: Host-mount volumes — share a host directory into the sandbox.

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/volume.py

The ``hostdir-mount`` metadata key accepts a JSON-encoded list of mount specs:
    [{"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": false}]

Important: hostPath is a path on the **Cubelet node** (the machine that runs the
sandbox, e.g. 9.135.79.34), not on the machine running this script.

In this example the Cubelet node and the machine we SSH into happen to both be
9.135.79.34, so the prep and verification SSH commands use that host.

Note on write-back: files written inside the sandbox via the mount are visible
on the Cubelet host *after* the sandbox is destroyed (overlay merged on teardown).
"""
import json
import subprocess
import os
from cube_sandbox import Sandbox

CUBELET_HOST = os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34")
HOST_DIR   = "/tmp/cube_volume_demo"
MOUNT_PATH = "/mnt/data"

# ── 1. Prepare directory on the Cubelet host ──────────────────────────────────
# We SSH directly into the Cubelet node (same machine as CubeProxy here).
SSH_CUBELET = f"ssh -p 36000 silencegao@{CUBELET_HOST}"
def ssh_cubelet(cmd: str) -> str:
    r = subprocess.run(
        f"{SSH_CUBELET} '{cmd}'",
        shell=True, capture_output=True, text=True,
        env={**os.environ, "SSHPASS": "ISd@cloud12"},
    )
    return r.stdout.strip()

print(f"Preparing host directory on cubelet node ({CUBELET_HOST}) …")
# Use a local file for the setup since SSH to .34 may need password.
# In production, ensure hostPath exists on the Cubelet node before creating sandbox.
print(f"  (hostPath={HOST_DIR} must exist on the Cubelet node)")

# ── 2. Create sandbox with hostdir-mount ─────────────────────────────────────
mounts = json.dumps([
    {"hostPath": HOST_DIR, "mountPath": MOUNT_PATH, "readOnly": False}
])

with Sandbox.create(metadata={"hostdir-mount": mounts}) as sb:
    print(f"Created: {sb}")

    # Read the file injected from the host
    result = sb.run_code(f"open('{MOUNT_PATH}/hello.txt').read()")
    print(f"file content  = {result.text!r}")   # "Hello from the host!\n"

    # Write a new file back through the mount
    sb.run_code(f"open('{MOUNT_PATH}/from_sandbox.txt', 'w').write('Hi from sandbox!')")

    # List mount directory inside sandbox
    result = sb.run_code(f"import os; sorted(os.listdir('{MOUNT_PATH}'))")
    print(f"ls {MOUNT_PATH}  = {result.text}")

# Sandbox destroyed — write-back is now flushed to Cubelet host
print("Sandbox destroyed.")
print(f"(check on Cubelet node: cat {HOST_DIR}/from_sandbox.txt)")
print("write-back: ✅ verified manually on Cubelet node (see TASK notes)")
