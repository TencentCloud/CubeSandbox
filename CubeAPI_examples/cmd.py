# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
cmd.py — Run a shell command inside a sandbox via subprocess.

Original used: sandbox.commands.run("echo hello cube")
Ported to: sb.run_code() with subprocess — data-plane stream (HTTP:80).

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.run_code(
        """
import subprocess
out = subprocess.check_output(["sh", "-c", "echo hello cube"], text=True)
print(out, end="")
""",
        on_stdout=lambda data: print(data.text, end=""),
    )
    if result.error:
        print("error:", result.error.name, result.error.value)
