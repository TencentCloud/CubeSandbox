# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Create a sandbox and run code.

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34   # only needed for remote clients
    python examples/create_and_run.py
"""
import os
from cubesandbox import Sandbox

# ── Option A: environment variables (recommended) ─────────────────────────
with Sandbox.create() as sb:
    print(f"Created: {sb}")

    # Variables persist across run_code calls within the same sandbox
    sb.run_code("import math")
    sb.run_code("x = math.pi * 2")
    result = sb.run_code("round(x, 4)")
    print(f"result.text   = {result.text!r}")     # "6.2832"

    # Capture stdout
    result = sb.run_code(
        "for i in range(3): print(f'item {i}')",
        on_stdout=lambda msg: print("  stdout:", msg.text, end=""),
    )
    print(f"logs.stdout   = {result.logs.stdout}")

    # Error handling
    result = sb.run_code("1 / 0")
    if result.error:
        print(f"error.name    = {result.error.name}")
        print(f"error.value   = {result.error.value}")

# Sandbox is automatically destroyed on __exit__
print("Sandbox destroyed.")


# ── Option B: explicit config ─────────────────────────────────────────────
from cubesandbox import Config, Sandbox

config = Config(
    api_url="http://9.135.79.34:3000",
    template_id="tpl-6265796cee124256b4dcd6a1",
    proxy_node_ip="9.135.79.34",  # bypass DNS for remote client
    timeout=120,
)

sb = Sandbox.create(config=config)
print(f"sandbox_id = {sb.sandbox_id}")
result = sb.run_code("sum(range(101))")
print(f"sum(1..100) = {result.text}")   # "5050"
sb.kill()
