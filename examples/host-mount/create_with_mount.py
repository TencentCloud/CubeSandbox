# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
create_with_mount.py — Create a sandbox with host directories mounted inside it.

This is a Cube Sandbox extension to the E2B API. Host mounts are specified via
the `metadata["host-mount"]` field as a JSON-encoded list of mount descriptors.

Each descriptor has three fields:
  hostPath  — absolute path on the Cubelet host to bind-mount
  mountPath — target path inside the sandbox VM
  readOnly  — whether the mount should be read-only (True) or read-write (False)

Use cases:
  - Provide large datasets to a sandbox without copying them in
  - Share a read-only model/config directory across many sandboxes
  - Write sandbox outputs directly to a host path for persistence
"""

import json
import os
import subprocess

from cubesandbox import Sandbox
from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Ensure the host directories exist before creating the sandbox.
# CubeMaster requires that bind-mount source paths already exist.
for d in ["/tmp/rw", "/tmp/ro"]:
    os.makedirs(d, exist_ok=True)
    # Write a small marker so `ls` shows something meaningful
    marker = os.path.join(d, "hello.txt")
    if not os.path.exists(marker):
        with open(marker, "w") as f:
            f.write(f"marker from host {d}\n")

with Sandbox.create(
    template=template_id,
    metadata={
        # host-mount is a JSON-encoded list; each entry maps a host path to a
        # path inside the sandbox VM.
        "host-mount": json.dumps([
            {
                "hostPath":  "/tmp/rw",   # host directory (must exist on the Cubelet node)
                "mountPath": "/mnt/rw",   # where it appears inside the sandbox
                "readOnly":  False,       # read-write mount
            },
            {
                "hostPath":  "/tmp/ro",   # host directory (must exist on the Cubelet node)
                "mountPath": "/mnt/ro",   # where it appears inside the sandbox
                "readOnly":  True,        # read-only mount
            },
        ])
    },
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info:", info)

    # Verify the mounts are visible inside the sandbox
    result = sandbox.commands.run("ls /mnt/rw /mnt/ro")
    print("mount contents:", result.stdout.strip())
