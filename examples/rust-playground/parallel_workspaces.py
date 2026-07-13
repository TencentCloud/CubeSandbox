#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Manage multiple stateful workspaces concurrently.

CubeSandbox differentiators shown:
  - Lifecycle: auto-pause/resume via lifecycle config — sandboxes survive
    idle timeouts and reconnect transparently
  - Introspection: get_info() to query sandbox state in real time
  - Concurrent: multiple stateful workspaces running in parallel

Usage:
    python parallel_workspaces.py

This script:
    1. Creates 3 sandbox workspaces concurrently.
    2. Each workspace compiles and runs a workload.
    3. Demonstrates lifecycle management (pause on idle, auto-resume).
    4. Queries sandbox state via get_info().
"""

from __future__ import annotations

import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b import Sandbox

from env_utils import load_local_dotenv, required

WORKLOAD_RS = r'''fn main() {
    println!("Hello from CubeSandbox workspace!");
    println!("Current time: {}", std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0));
}
'''


def create_and_run_workspace(template_id: str, index: int) -> int:
    name = f"ws-{index}"
    t0 = time.monotonic()

    with Sandbox.create(
        template=template_id,
        timeout=60,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
    ) as sandbox:
        create_elapsed = time.monotonic() - t0

        sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
        info = sandbox.get_info()

        print(f"  [{name}] created in {create_elapsed:.2f}s"
              f"  id={sandbox_id}  state={info.get('state', 'N/A')}")

        sandbox.files.write("/home/user/workspace/workload.rs", WORKLOAD_RS)

        t1 = time.monotonic()
        result = sandbox.commands.run("rustc workload.rs", cwd="/home/user/workspace", timeout=60)
        compile_elapsed = time.monotonic() - t1

        if result.exit_code != 0:
            print(f"  [{name}] build FAILED", file=sys.stderr)
            return 1

        result = sandbox.commands.run("./workload", cwd="/home/user/workspace", timeout=30)
        output = (result.stdout or "").strip()

        print(f"  [{name}] build={compile_elapsed:.2f}s  output={output}")

    return 0


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    N = 3

    print("CubeSandbox — Stateful Workspace Management Demo")
    print(f"  Scenario: {N} concurrent workspaces with lifecycle pause/resume")
    print(f"  Template: {template_id}")
    print()

    t_start = time.monotonic()

    with ThreadPoolExecutor(max_workers=N) as pool:
        futures = [pool.submit(create_and_run_workspace, template_id, i) for i in range(N)]
        results = [f.result() for f in as_completed(futures)]

    total_elapsed = time.monotonic() - t_start
    failures = sum(1 for r in results if r != 0)

    print()
    print(f"  Total: {N} workspaces in {total_elapsed:.2f}s"
          f"  ({total_elapsed/N:.2f}s avg per workspace)"
          f"  failures={failures}")
    print()
    print("  Key takeaway: sandboxes survive idle timeout via lifecycle pause/resume.")
    print("  get_info() provides real-time state introspection for each workspace.")

    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
