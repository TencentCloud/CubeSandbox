# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
list.py — List all running sandboxes (v1 + v2 APIs).

Original used: e2b Sandbox.list() with SandboxQuery pagination.
Ported to: cubesandbox Sandbox.list() / Sandbox.list_v2() — no pagination wrapper,
           returns all sandboxes directly.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

# v1: GET /sandboxes
sandboxes_v1 = Sandbox.list()
print("total running sandboxes (v1): %d" % len(sandboxes_v1))
for sb in sandboxes_v1:
    print("  sandbox_id=%-36s template=%s" % (
        sb.get("sandboxID", "?"),
        sb.get("templateID", "?"),
    ))

print()

# v2: GET /v2/sandboxes
sandboxes_v2 = Sandbox.list_v2()
print("total running sandboxes (v2): %d" % len(sandboxes_v2))
for sb in sandboxes_v2:
    print("  sandbox_id=%-36s template=%s" % (
        sb.get("sandboxID", "?"),
        sb.get("templateID", "?"),
    ))
