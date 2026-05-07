# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
create.py — Create a sandbox and print its info.

Ported from e2b_code_interpreter to cubesandbox v0.1.0.

Env vars:
    CUBE_API_URL       e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   e.g. tpl-6265796cee124256b4dcd6a1
    CUBE_PROXY_NODE_IP e.g. 9.135.79.34  (data-plane: HTTP:80 / HTTPS:443)
"""
import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
