#!/usr/bin/env python3
"""Verification runner: runs all demo scripts against mock SDK and captures output."""

from __future__ import annotations

import io
import os
import sys
import time
import contextlib
from pathlib import Path

# Inject mock SDK before any demo imports
sys.path.insert(0, str(Path(__file__).parent))
import mock_sdk
mock_sdk.install()

# Now safe to import the demos
demo_dir = Path(__file__).parent.parent
sys.path.insert(0, str(demo_dir))

# Set required env vars for all demos
os.environ["CUBE_TEMPLATE_ID"] = "template-sandbox-patterns-v1"
os.environ["E2B_API_URL"] = "http://127.0.0.1:3000"
os.environ["E2B_API_KEY"] = "mock-key-for-verification"


def run_demo(name: str, module_path: str) -> str:
    print(f"\n{'='*70}")
    print(f"  RUNNING: {name}")
    print(f"{'='*70}\n")

    buf = io.StringIO()
    with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
        try:
            import importlib
            mod = importlib.import_module(module_path)
            rc = mod.main()
            if rc:
                print(f"\n  ✗ FAILED with exit code {rc}")
            else:
                print(f"\n  ✓ PASSED")
        except Exception as e:
            print(f"\n  ✗ EXCEPTION: {type(e).__name__}: {e}", file=sys.stderr)
            import traceback
            traceback.print_exc(file=sys.stderr)

    output = buf.getvalue()
    print(output)
    return output


def main() -> int:
    timestamp = time.strftime("%Y%m%d-%H%M%S")
    log_dir = Path(demo_dir) / "verification-logs"
    log_dir.mkdir(parents=True, exist_ok=True)

    demos = [
        ("parallel_workspaces.py  (stateful workspaces with lifecycle)", "parallel_workspaces"),
        ("network_isolation.py  (egress policy enforcement)", "network_isolation"),
        ("snapshot_driven_dev.py  (checkpoint-driven development)", "snapshot_driven_dev"),
        ("multi_container.py  (multi-sandbox collaboration)", "multi_container"),
    ]

    all_output = ""
    results: dict[str, bool] = {}
    all_pass = True

    all_output += f"CubeSandbox Scenario Demos — Verification Report\n"
    all_output += f"Generated: {time.strftime('%Y-%m-%d %H:%M:%S')}\n"
    all_output += f"Git commit: {os.popen('git log --oneline -1 2>/dev/null').read().strip()}\n"
    all_output += f"Mock SDK: mock_sdk.py\n"
    all_output += f"{'='*70}\n"

    for name, module in demos:
        output = run_demo(name, module)
        all_output += f"\n{'─'*70}\n"
        all_output += f"DEMO: {name}\n"
        all_output += f"{'─'*70}\n"
        all_output += output

        demo_pass = "✗ FAILED" not in output and "EXCEPTION" not in output
        results[module] = demo_pass
        if not demo_pass:
            all_pass = False

    # Write full log
    log_file = log_dir / f"verification-{timestamp}.log"
    log_file.write_text(all_output)
    print(f"\nLog saved to: {log_file}")

    # Write summary
    pass_str = "PASS ✓" if all_pass else "FAIL ✗"
    summary = f"""# CubeSandbox Scenario Demos — Verification Results

**Date:** {time.strftime('%Y-%m-%d %H:%M:%S')}
**Commit:** {os.popen('git log --oneline -1 2>/dev/null').read().strip()}
**Overall: {pass_str}**

## Results

| Demo | Scenario | Status |
|------|----------|--------|
| parallel_workspaces.py | Stateful workspace lifecycle | {'PASS ✓' if results.get('parallel_workspaces', False) else 'FAIL ✗'} |
| network_isolation.py   | Egress policy enforcement  | {'PASS ✓' if results.get('network_isolation', False) else 'FAIL ✗'} |
| snapshot_driven_dev.py | Checkpoint-driven dev      | {'PASS ✓' if results.get('snapshot_driven_dev', False) else 'FAIL ✗'} |
| multi_container.py     | Multi-sandbox collaboration | {'PASS ✓' if results.get('multi_container', False) else 'FAIL ✗'} |

## CubeSandbox Capabilities Covered

| Capability | Demo |
|------------|------|
| Lifecycle (pause/resume) | parallel_workspaces, network_isolation, snapshot_driven_dev, multi_container |
| Introspection (get_info) | parallel_workspaces, network_isolation, snapshot_driven_dev, multi_container |
| Egress policy (allow_internet_access) | network_isolation, multi_container |
| Snapshot outlives sandbox | snapshot_driven_dev |
| Instant rollback (~100ms) | snapshot_driven_dev |
| Clone (sb.clone(n=N)) | snapshot_driven_dev |
| Snapshot management (list/delete) | snapshot_driven_dev |
| Cross-sandbox artifact transfer | multi_container |
| Role-based network isolation | multi_container |
"""
    summary_file = log_dir / "verification-summary.md"
    summary_file.write_text(summary)
    print(f"Summary saved to: {summary_file}")

    return 0 if all_pass else 1


if __name__ == "__main__":
    sys.exit(main())
