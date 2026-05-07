# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Kernel Context — share variable state across run_code calls.

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/context.py

A Context is a server-side kernel namespace created via envd's POST /contexts.
Calling sandbox.create_context() returns a Context object with a server-assigned id.
Pass it to run_code() to keep variables alive between calls.
"""
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    print(f"Created: {sb}")

    # ── A: without context — variables do NOT persist ─────────────────────────
    print("\n--- without context ---")
    sb.run_code("x = 100")
    result = sb.run_code("x")          # NameError expected
    if result.error:
        print(f"expected error  : {result.error.name}: {result.error.value}")
    else:
        print(f"result.text     = {result.text!r}")

    # ── B: with a shared context — variables DO persist ───────────────────────
    print("\n--- with shared context ---")
    ctx = sb.create_context()
    print(f"context id      = {ctx.id!r}")

    sb.run_code("x = 100",         context=ctx)
    sb.run_code("y = x * 2",       context=ctx)
    result = sb.run_code("x + y",  context=ctx)
    print(f"x=100, y=x*2, x+y = {result.text!r}")   # "300"

    # Accumulate state across multiple steps
    sb.run_code("total = 0",           context=ctx)
    for i in range(1, 6):
        sb.run_code(f"total += {i}",   context=ctx)
    result = sb.run_code("total",      context=ctx)
    print(f"sum(1..5)         = {result.text!r}")    # "15"

    # ── C: two independent contexts — namespaces are isolated ─────────────────
    print("\n--- two independent contexts ---")
    ctx_a = sb.create_context()
    ctx_b = sb.create_context()

    sb.run_code("value = 'Alice'", context=ctx_a)
    sb.run_code("value = 'Bob'",   context=ctx_b)

    result_a = sb.run_code("value", context=ctx_a)
    result_b = sb.run_code("value", context=ctx_b)
    print(f"ctx_a value = {result_a.text!r}")   # "Alice"
    print(f"ctx_b value = {result_b.text!r}")   # "Bob"

    # ── D: streaming with context ─────────────────────────────────────────────
    print("\n--- streaming with context ---")
    ctx_d = sb.create_context()
    sb.run_code("items = list(range(4))", context=ctx_d)
    result = sb.run_code(
        "for i in items: print(f'item {i}')",
        context=ctx_d,
        on_stdout=lambda m: print("  stdout:", m.text, end=""),
    )
    print(f"logs.stdout = {result.logs.stdout}")

    # ── E: cleanup contexts explicitly ────────────────────────────────────────
    print("\n--- cleanup ---")
    sb.delete_context(ctx_a)
    sb.delete_context(ctx_b)
    sb.delete_context(ctx_d)
    print("contexts deleted")

print("\nSandbox destroyed.")
