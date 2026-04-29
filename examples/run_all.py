"""
Run all examples and collect output into examples/output.md.

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
    ("create_and_run", "examples/create_and_run.py"),
    ("lifecycle",      "examples/lifecycle.py"),
    ("volume",         "examples/volume.py"),
    ("context",        "examples/context.py"),
    ("network_policy", "examples/network_policy.py"),
]

OUTPUT_MD = "examples/output.md"

results = []
env = os.environ.copy()
env["PYTHONPATH"] = "."

for name, path in EXAMPLES:
    print(f"\n{'='*50}")
    print(f"Running: {name}")
    print('='*50)
    start = datetime.now()
    try:
        r = subprocess.run(
            [sys.executable, path],
            capture_output=True, text=True, timeout=120, env=env
        )
        elapsed = (datetime.now() - start).total_seconds()
        status = "✅ PASS" if r.returncode == 0 else f"❌ FAIL (exit {r.returncode})"
        output = r.stdout + (("\n[stderr]\n" + r.stderr) if r.stderr.strip() else "")
    except subprocess.TimeoutExpired:
        elapsed = 120
        status = "⏱️ TIMEOUT"
        output = "Timed out after 120s"
    except Exception as e:
        elapsed = 0
        status = f"❌ ERROR: {e}"
        output = str(e)

    print(f"Status : {status}")
    print(f"Time   : {elapsed:.1f}s")
    print(f"Output :\n{output}")
    results.append((name, status, elapsed, output))

# Write output.md
with open(OUTPUT_MD, "w") as f:
    f.write(f"# cube-e2b Example Run Results\n\n")
    f.write(f"**Date**: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\n\n")
    f.write(f"**Env**: `CUBE_API_URL={os.environ.get('CUBE_API_URL')}` "
            f"`CUBE_PROXY_NODE_IP={os.environ.get('CUBE_PROXY_NODE_IP')}`\n\n")
    f.write("| Example | Status | Time |\n")
    f.write("|---------|--------|------|\n")
    for name, status, elapsed, _ in results:
        f.write(f"| `{name}` | {status} | {elapsed:.1f}s |\n")
    f.write("\n---\n\n")
    for name, status, elapsed, output in results:
        f.write(f"## {name}\n\n")
        f.write(f"**Status**: {status}  **Time**: {elapsed:.1f}s\n\n")
        f.write("```\n")
        f.write(output.strip())
        f.write("\n```\n\n")

print(f"\n\nResults written to {OUTPUT_MD}")
