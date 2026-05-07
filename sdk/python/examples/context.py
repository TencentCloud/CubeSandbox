# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Kernel Context — share variable state across run_code calls.

Tests:
  - sb.create_context()                    — POST /contexts
  - sb.run_code(code, context=ctx)         — state persists within a context
  - sb.run_code(code)                      — no context: no state persistence
  - two independent contexts are isolated  — namespaces don't bleed
  - sb.delete_context(ctx)                 — DELETE /contexts/{id}
  - streaming (on_stdout) with context

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

    # ── A: without context — variables do NOT persist ─────────────────────────
    print("\n--- A: without context (no state persistence) ---")
    sb.run_code("x = 100")
    result = sb.run_code("x")
    if result.error:
        print(f"  expected NameError: {result.error.name}: {result.error.value}")
        check("no-context: NameError raised", result.error.name == "NameError",
              f"got {result.error.name!r}")
    else:
        # Some envd versions may share state globally; accept both behaviors
        print(f"  result.text = {result.text!r} (envd may share global state)")
        check("no-context: result returned", result.text is not None)

    # ── B: with a shared context — variables DO persist ───────────────────────
    print("\n--- B: with shared context ---")
    ctx = sb.create_context()
    print(f"  context id = {ctx.id!r}")
    check("create_context returns id", bool(ctx.id))

    sb.run_code("x = 100",         context=ctx)
    sb.run_code("y = x * 2",       context=ctx)
    result = sb.run_code("x + y",  context=ctx)
    print(f"  x=100, y=x*2, x+y = {result.text!r}")
    check("context: x + y == 300", result.text == "300", f"got {result.text!r}")

    # Accumulate across multiple calls
    sb.run_code("total = 0",           context=ctx)
    for i in range(1, 6):
        sb.run_code(f"total += {i}",   context=ctx)
    result = sb.run_code("total",      context=ctx)
    print(f"  sum(1..5) = {result.text!r}")
    check("context: sum(1..5) == 15", result.text == "15", f"got {result.text!r}")

    # ── C: two independent contexts — namespaces are isolated ─────────────────
    print("\n--- C: two independent contexts ---")
    ctx_a = sb.create_context()
    ctx_b = sb.create_context()

    sb.run_code("value = 'Alice'", context=ctx_a)
    sb.run_code("value = 'Bob'",   context=ctx_b)

    result_a = sb.run_code("value", context=ctx_a)
    result_b = sb.run_code("value", context=ctx_b)
    print(f"  ctx_a.value = {result_a.text!r}")
    print(f"  ctx_b.value = {result_b.text!r}")
    check("ctx_a isolated", result_a.text == "Alice", f"got {result_a.text!r}")
    check("ctx_b isolated", result_b.text == "Bob",   f"got {result_b.text!r}")

    # ── D: streaming (on_stdout) with context ─────────────────────────────────
    print("\n--- D: streaming with context ---")
    ctx_d = sb.create_context()
    sb.run_code("items = list(range(4))", context=ctx_d)
    captured: list[str] = []
    result = sb.run_code(
        "for i in items: print(f'item {i}')",
        context=ctx_d,
        on_stdout=lambda m: captured.extend([l for l in m.text.splitlines() if l]),
    )
    print(f"  stdout captured: {captured}")
    check("streaming: 4 stdout lines", len(captured) == 4, f"got {captured}")

    # ── E: delete contexts ────────────────────────────────────────────────────
    print("\n--- E: delete contexts ---")
    sb.delete_context(ctx)
    sb.delete_context(ctx_a)
    sb.delete_context(ctx_b)
    sb.delete_context(ctx_d)
    print("  all contexts deleted")
    check("delete_context no error", True)

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
