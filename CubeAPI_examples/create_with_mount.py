# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
create_with_mount.py — Create sandbox with host-directory mounts (rw + ro).

Original used: metadata={"host-mount": ...}
NOTE: CubeAPI uses "hostdir-mount" as the annotation key (not "host-mount").
      This example uses the correct key.

Two mounts:
  /tmp/rw  → /mnt/rw   readOnly=False  (read + write)
  /tmp/ro  → /mnt/ro   readOnly=True   (read only, write must fail)

hostPath directories must exist on the Cubelet node (CUBE_PROXY_NODE_IP).
This script pre-creates them via a separate sandbox running shell commands.

Data-plane stream (HTTP:80) is used for all run_code calls.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP  (HTTP port 80; also the Cubelet node)
"""
import os
import sys
import json
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

HOST_RW = "/tmp/rw"
HOST_RO = "/tmp/ro"

# ── Step 0: host dirs must exist on the Cubelet node (CUBE_PROXY_NODE_IP) ────
# Pre-created on 9.135.79.34 via:
#   mkdir -p /tmp/rw /tmp/ro
#   echo 'rw seed from host' > /tmp/rw/seed.txt
#   echo 'ro seed from host' > /tmp/ro/seed.txt
print("=== Step 0: host directories (pre-created on Cubelet node) ===")
print(f"  Cubelet node: {os.environ.get('CUBE_PROXY_NODE_IP', '9.135.79.34')}")
print(f"  hostPath rw: {HOST_RW}/seed.txt")
print(f"  hostPath ro: {HOST_RO}/seed.txt")

# ── Step 1: create sandbox with two mounts ───────────────────────────────────
print("\n=== Step 1: create sandbox with hostdir-mount ===")
mounts = json.dumps([
    {"hostPath": HOST_RW, "mountPath": "/mnt/rw", "readOnly": False},
    {"hostPath": HOST_RO, "mountPath": "/mnt/ro", "readOnly": True},
])

with Sandbox.create(
    template=template_id,
    metadata={"hostdir-mount": mounts},
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)

    # ── rw mount: read seed, write new file ──────────────────────────────────
    print("\n--- rw mount ---")
    r = sandbox.run_code("""
import os
print("ls /mnt/rw:", os.listdir("/mnt/rw"))
content = open("/mnt/rw/seed.txt").read() if os.path.exists("/mnt/rw/seed.txt") else "(no seed)"
print("seed.txt:", repr(content))
open("/mnt/rw/from_sandbox.txt", "w").write("written by sandbox")
print("wrote from_sandbox.txt")
""")
    for line in r.logs.stdout:
        print(" ", line.strip())
    if r.error:
        print("  error:", r.error.name, r.error.value)

    # ── ro mount: read ok, write must fail ───────────────────────────────────
    print("\n--- ro mount ---")
    r = sandbox.run_code("""
import os
print("ls /mnt/ro:", os.listdir("/mnt/ro"))
content = open("/mnt/ro/seed.txt").read() if os.path.exists("/mnt/ro/seed.txt") else "(no seed)"
print("seed.txt:", repr(content))
try:
    open("/mnt/ro/should_fail.txt", "w").write("x")
    print("ERROR: write to ro mount succeeded unexpectedly")
except OSError as e:
    print(f"write blocked as expected: {type(e).__name__}: {e}")
""")
    for line in r.logs.stdout:
        print(" ", line.strip())
    if r.error:
        print("  error:", r.error.name, r.error.value)

print("\nsandbox destroyed — rw write-back flushed to Cubelet host on teardown")
