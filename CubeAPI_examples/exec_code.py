# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
exec_code.py — Execute Python code inside a sandbox and stream stdout.

Ported from e2b_code_interpreter to cubesandbox v0.1.0.

Data-plane path (envd stream):
    POST http://<CUBE_PROXY_NODE_IP>:80/execute
    Host: 49999-<sandboxID>.cube.app
    → IPOverrideTransport routes TCP to CUBE_PROXY_NODE_IP:80 (HTTP)

Env vars:
    CUBE_API_URL           management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID       sandbox template
    CUBE_PROXY_NODE_IP     data-plane IP (HTTP port 80)
    CUBE_PROXY_PORT_HTTP   data-plane port, default 80
"""
import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

python_code = """
print("hello cube")
"""

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.run_code(
        python_code,
        on_stdout=lambda data: print(data.text, end=""),
    )
    print(result)
