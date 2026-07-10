#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate CubeSandbox's instant creation and concurrent sandboxes.

CubeSandbox differentiators shown:
  - Instant: sandbox creation in seconds, measured with wall-clock timing
  - Concurrent: multiple sandboxes running in parallel
  - Lifecycle: auto-pause/resume via lifecycle config

Usage:
    python hello_world.py

This script:
    1. Creates 3 sandboxes concurrently from a Rust-enabled template.
    2. Measures sandbox creation time (CubeSandbox's instant startup).
    3. Compiles and runs a Rust program in each sandbox simultaneously.
    4. Inspects sandbox state via get_info().
"""

from __future__ import annotations

import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b import Sandbox

from env_utils import load_local_dotenv, required

HELLO_RS = r'''fn main() {
    println!("Hello from CubeSandbox Rust playground!");
    println!("Current time: {}", std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0));
}
'''


def create_and_run_sandbox(template_id: str, index: int) -> int:
    name = f"sb-{index}"
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

        # Write source
        sandbox.files.write("/home/user/workspace/hello.rs", HELLO_RS)

        # Compile
        t1 = time.monotonic()
        result = sandbox.commands.run("rustc hello.rs", cwd="/home/user/workspace", timeout=60)
        compile_elapsed = time.monotonic() - t1

        if result.exit_code != 0:
            print(f"  [{name}] compile FAILED", file=sys.stderr)
            return 1

        # Run
        result = sandbox.commands.run("./hello", cwd="/home/user/workspace", timeout=30)
        output = (result.stdout or "").strip()

        print(f"  [{name}] compile={compile_elapsed:.2f}s  output={output}")

    return 0


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    N = 3

    print(f"CubeSandbox — Instant + Concurrent Demo ({N} sandboxes)")
    print(f"Template: {template_id}")
    print()

    t_start = time.monotonic()

    with ThreadPoolExecutor(max_workers=N) as pool:
        futures = [pool.submit(create_and_run_sandbox, template_id, i) for i in range(N)]
        results = [f.result() for f in as_completed(futures)]

    total_elapsed = time.monotonic() - t_start
    failures = sum(1 for r in results if r != 0)

    print()
    print(f"Total: {N} sandboxes in {total_elapsed:.2f}s"
          f"  ({total_elapsed/N:.2f}s avg per sandbox)"
          f"  failures={failures}")

    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
