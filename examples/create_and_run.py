# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Create a sandbox and run code.

Tests:
  - Sandbox.create()
  - sb.run_code() — basic result, stdout streaming, error handling, env_vars
  - sb.kill() (implicit via context manager)

Usage:
    export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
    python examples/create_and_run.py
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox, Config

failures: list[str] = []


def check(tag: str, condition: bool, detail: str = "") -> None:
    if condition:
        print(f"  ✅ {tag}")
    else:
        msg = f"{tag}: {detail}" if detail else tag
        print(f"  ❌ {msg}")
        failures.append(msg)


# ── 1. create + basic run_code ───────────────────────────────────────────────
print("=== create + run_code ===")
with Sandbox.create() as sb:
    print(f"  created: {sb}")
    check("sandbox_id not empty", bool(sb.sandbox_id))

    # Variables persist within the same sandbox (no context)
    sb.run_code("import math")
    sb.run_code("x = math.pi * 2")
    result = sb.run_code("round(x, 4)")
    check("math result", result.text == "6.2832", f"got {result.text!r}")

    # ── stdout streaming ─────────────────────────────────────────────
    # Note: envd may deliver multiple print() lines as one callback event
    # (e.g. 'item 0\nitem 1\nitem 2\n'). Split before counting.
    captured: list[str] = []
    sb.run_code(
        "for i in range(3): print(f'item {i}')",
        on_stdout=lambda msg: captured.extend(
            [l for l in msg.text.splitlines() if l]
        ),
    )
    check("stdout lines", len(captured) == 3, f"got {captured}")

    # ── stderr streaming ─────────────────────────────────────────────
    stderr_lines: list[str] = []
    sb.run_code(
        "import sys; sys.stderr.write('warn\\n')",
        on_stderr=lambda msg: stderr_lines.append(msg.text),
    )
    check("stderr callback", len(stderr_lines) > 0, f"got {stderr_lines}")

    # ── error handling ────────────────────────────────────────────────
    result = sb.run_code("1 / 0")
    check("error.name", result.error is not None and result.error.name == "ZeroDivisionError",
          f"error={result.error}")

    # ── env_vars ──────────────────────────────────────────────────────
    result = sb.run_code("import os; os.environ.get('MY_VAR', 'missing')",
                         envs={"MY_VAR": "hello"})
    check("env_vars", result.text == "hello", f"got {result.text!r}")

print("Sandbox destroyed.\n")

# ── 2. explicit Config ───────────────────────────────────────────────────────
print("=== explicit Config ===")
config = Config(
    api_url=os.environ["CUBE_API_URL"],
    template_id=os.environ["CUBE_TEMPLATE_ID"],
    proxy_node_ip=os.environ["CUBE_PROXY_NODE_IP"],
    timeout=120,
)
sb2 = Sandbox.create(config=config)
result2 = sb2.run_code("sum(range(101))")
check("sum(1..100)", result2.text == "5050", f"got {result2.text!r}")
sb2.kill()
print("  sandbox killed\n")

# ── summary ──────────────────────────────────────────────────────────────────
print("=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
