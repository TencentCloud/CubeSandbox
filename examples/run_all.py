# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Run all examples and collect output into examples/output.md.

Covers all CubeAPI-supported features:
  - GET  /health
  - GET  /sandboxes (v1)
  - GET  /v2/sandboxes (v2)
  - POST /sandboxes (create)
  - GET  /sandboxes/{id} (get_info)
  - DELETE /sandboxes/{id} (kill)
  - POST /sandboxes/{id}/pause
  - POST /sandboxes/{id}/resume (deprecated)
  - POST /sandboxes/{id}/connect
  - envd: POST /execute (run_code + streaming + env_vars)
  - envd: POST /contexts + DELETE /contexts/{id}
  - metadata: network-policy (allow-all / deny-all / custom)
  - metadata: hostdir-mount (readOnly=false / readOnly=true)

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/run_all.py
"""
import subprocess
import sys
import os
from datetime import datetime

EXAMPLES = [
    ("list_and_health",  "examples/list_and_health.py"),
    ("create_and_run",   "examples/create_and_run.py"),
    ("lifecycle",        "examples/lifecycle.py"),
    ("context",          "examples/context.py"),
    ("volume",           "examples/volume.py"),
    ("network_policy",   "examples/network_policy.py"),
]

OUTPUT_MD = "examples/output.md"

results = []
env = os.environ.copy()
env["PYTHONPATH"] = "."

for name, path in EXAMPLES:
    print(f"\n{'='*55}")
    print(f"Running: {name}")
    print('='*55)
    start = datetime.now()
    try:
        r = subprocess.run(
            [sys.executable, path],
            capture_output=True, text=True, timeout=180, env=env,
        )
        elapsed = (datetime.now() - start).total_seconds()
        status = "✅ PASS" if r.returncode == 0 else f"❌ FAIL (exit {r.returncode})"
        output = r.stdout + (("\n[stderr]\n" + r.stderr) if r.stderr.strip() else "")
    except subprocess.TimeoutExpired:
        elapsed = 180
        status = "⏱️ TIMEOUT"
        output = "Timed out after 180s"
    except Exception as e:
        elapsed = 0
        status = f"❌ ERROR: {e}"
        output = str(e)

    print(f"Status : {status}")
    print(f"Time   : {elapsed:.1f}s")
    print(f"Output :\n{output}")
    results.append((name, status, elapsed, output))

# ── Write output.md ───────────────────────────────────────────────────────────
with open(OUTPUT_MD, "w") as f:
    f.write("# CubeSandbox Example Run Results\n\n")
    f.write(f"**SDK**: cubesandbox v0.1.0  \n")
    f.write(f"**Date**: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}  \n")
    f.write(f"**Env**: `CUBE_API_URL={os.environ.get('CUBE_API_URL')}`  "
            f"`CUBE_PROXY_NODE_IP={os.environ.get('CUBE_PROXY_NODE_IP')}`\n\n")

    f.write("## Summary\n\n")
    f.write("| Example | Status | Time |\n")
    f.write("|---------|--------|------|\n")
    for name, status, elapsed, _ in results:
        f.write(f"| `{name}` | {status} | {elapsed:.1f}s |\n")

    total   = len(results)
    passed  = sum(1 for _, s, _, _ in results if "PASS" in s)
    failed  = total - passed
    f.write(f"\n**Total**: {total} | **Pass**: {passed} | **Fail**: {failed}\n\n")
    f.write("---\n\n")

    for name, status, elapsed, output in results:
        f.write(f"## {name}\n\n")
        f.write(f"**Status**: {status}  **Time**: {elapsed:.1f}s\n\n")
        f.write("```\n")
        f.write(output.strip())
        f.write("\n```\n\n")

print(f"\n\nResults written to {OUTPUT_MD}")

# Exit non-zero if any example failed
if any("FAIL" in s or "TIMEOUT" in s or "ERROR" in s for _, s, _, _ in results):
    sys.exit(1)
