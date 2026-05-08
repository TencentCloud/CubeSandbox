# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Variable persistence within a sandbox.

Variables assigned in one run_code call persist for the full lifetime of the
sandbox — no separate context object is needed.

  - sb.run_code("x = 100") then sb.run_code("x") returns 100
  - state accumulates across calls
  - each sandbox has its own isolated namespace

NOTE: create_context / delete_context methods exist in the SDK but the
server-side /contexts API is not yet implemented (envd returns HTTP 404).
Do NOT call create_context / delete_context in examples or production code.

Usage:
    export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
    python examples/context.py
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

failures: list[str] = []


def check(tag: str, condition: bool, detail: str = "") -> None:
    if condition:
        print(f"  ✅ {tag}")
    else:
        msg = f"{tag}: {detail}" if detail else tag
        print(f"  ❌ {msg}")
        failures.append(msg)


with Sandbox.create() as sb:
    print(f"Created: {sb}")

    # ── A: variables persist across run_code calls within same sandbox ────────
    print("\n--- A: variable persistence ---")
    sb.run_code("x = 100")
    result = sb.run_code("x")
    print(f"  x = {result.text!r}")
    check("x persists across calls", result.text == "100", f"got {result.text!r}")

    # ── B: accumulate state across multiple calls ─────────────────────────────
    print("\n--- B: accumulate state ---")
    sb.run_code("total = 0")
    for i in range(1, 6):
        sb.run_code(f"total += {i}")
    result = sb.run_code("total")
    print(f"  sum(1..5) = {result.text!r}")
    check("sum(1..5) == 15", result.text == "15", f"got {result.text!r}")

    # ── C: complex state (list / dict) persists ───────────────────────────────
    print("\n--- C: complex state persists ---")
    sb.run_code("items = []")
    for v in ["a", "b", "c"]:
        sb.run_code(f"items.append('{v}')")
    result = sb.run_code("','.join(items)")
    print(f"  items = {result.text!r}")
    check("list accumulation", result.text == "a,b,c", f"got {result.text!r}")

    # ── D: streaming with persistent state ───────────────────────────────────
    print("\n--- D: streaming with persistent state ---")
    sb.run_code("seq = list(range(4))")
    captured: list[str] = []
    result = sb.run_code(
        "for i in seq: print(f'item {i}')",
        on_stdout=lambda m: captured.extend([l for l in m.text.splitlines() if l]),
    )
    print(f"  stdout captured: {captured}")
    check("streaming: 4 stdout lines", len(captured) == 4, f"got {captured}")

print("\nSandbox destroyed.")

# ── summary ──────────────────────────────────────────────────────────────────
print("\n" + "=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
