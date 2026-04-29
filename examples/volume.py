"""
Example: Host-mount volumes — share a host directory into the sandbox.

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/volume.py

The ``host-mount`` metadata key accepts a JSON-encoded list of mount specs:
    [{"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": false}]

Note: hostPath is resolved on the CubeProxy/CubeMaster host (9.135.79.34),
NOT on the machine running this script.  We prepare the directory via
the CubeAPI management endpoint so the test is self-contained.
"""
import json
import subprocess
import os
from cube_e2b import Sandbox

PROXY_HOST = os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34")
HOST_DIR   = "/tmp/cube_volume_demo"
MOUNT_PATH = "/mnt/data"

# ── 1. Prepare directory on the CubeMaster/host machine via SSH ───────────────
SSH_CMD = f"ssh -p 36000 cube-devcloud"
def ssh(cmd: str) -> str:
    r = subprocess.run(f"{SSH_CMD} '{cmd}'", shell=True, capture_output=True, text=True)
    return r.stdout.strip()

print("Preparing host directory on cube-devcloud …")
ssh(f"mkdir -p {HOST_DIR} && echo 'Hello from the host!' > {HOST_DIR}/hello.txt")
print(f"  wrote hello.txt on cube-devcloud:{HOST_DIR}")

# ── 2. Create sandbox with host-mount ────────────────────────────────────────
mounts = json.dumps([
    {"hostPath": HOST_DIR, "mountPath": MOUNT_PATH, "readOnly": False}
])

with Sandbox.create(metadata={"host-mount": mounts}) as sb:
    print(f"Created: {sb}")

    # Read the file injected from the host
    result = sb.run_code(f"open('{MOUNT_PATH}/hello.txt').read()")
    print(f"file content  = {result.text!r}")   # "Hello from the host!\n"

    # Write a new file back through the mount
    sb.run_code(f"open('{MOUNT_PATH}/from_sandbox.txt', 'w').write('Hi from sandbox!')")

    # Verify sandbox wrote back to host
    written = ssh(f"cat {HOST_DIR}/from_sandbox.txt 2>/dev/null || echo '__MISSING__'")
    print(f"host sees     = {written!r}")        # "Hi from sandbox!"

    # List mount directory
    result = sb.run_code(f"import os; sorted(os.listdir('{MOUNT_PATH}'))")
    print(f"ls {MOUNT_PATH}  = {result.text}")

    # Cleanup host dir
    ssh(f"rm -rf {HOST_DIR}")

print("Sandbox destroyed.")
