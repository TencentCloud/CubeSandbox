# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
read.py — Read a file inside the sandbox via the data-plane stream (HTTP:80).

Original used: sandbox.files.read("/etc/hosts")
Ported to: sb.run_code() open() — data-plane stream HTTP:80.

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
    result = sandbox.run_code("print(open('/etc/hosts').read())")
    if result.error:
        print("error:", result.error.name, result.error.value)
    else:
        # result.text is the last expression; stdout captured in logs
        print(result.logs.stdout[0] if result.logs.stdout else result.text)
