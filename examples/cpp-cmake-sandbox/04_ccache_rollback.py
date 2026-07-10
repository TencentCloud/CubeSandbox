# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 04 (native cubesandbox SDK): in-place rollback to a warm-cache snapshot.
#
# Unlike 03 (which clones a NEW sandbox from the snapshot), this rolls the SAME
# sandbox back to the moment the compile cache was warm — then shows a rebuild
# hitting the cache. Handy for "reset my workspace but keep my build cache"
# and long-running / checkpoint-resume workflows.

import time

from cubesandbox import Sandbox

from env import TEMPLATE_ID
from seed import SANDBOX_PROJECT_DIR, push_project

BUILD_CMD = (
    f"cd {SANDBOX_PROJECT_DIR} "
    "&& cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release "
    "&& cmake --build build"
)


def run(sb, cmd, *, timeout=300):
    """Run a command, echo its output, and raise on a non-zero exit code."""
    result = sb.commands.run(cmd, timeout=timeout)
    if result.stdout:
        print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
    if result.exit_code != 0:
        print(result.stderr)
        raise SystemExit(f"command failed (exit={result.exit_code}): {cmd}")
    return result


snapshot_id = None

sb = Sandbox.create(template=TEMPLATE_ID)
try:
    print(f"sandbox: {sb.sandbox_id}")
    push_project(sb)

    # Warm the cache with one full build, then snapshot this state.
    run(sb, "ccache --zero-stats")
    run(sb, BUILD_CMD)
    snapshot = sb.create_snapshot()
    snapshot_id = snapshot.snapshot_id
    print(f"snapshot (warm cache) created: {snapshot_id}")

    # Simulate a dirty / broken workspace after the snapshot.
    run(sb, f"rm -rf {SANDBOX_PROJECT_DIR}/build")
    run(sb, f"rm -rf {SANDBOX_PROJECT_DIR}/src")
    print("workspace mutated (build/ and src/ removed)")

    # Roll the SAME sandbox back to the warm-cache snapshot.
    sb.rollback(snapshot_id)
    print("rolled back to warm-cache snapshot")

    # Rebuild after rollback: sources are back and ccache is warm -> cache hits.
    run(sb, f"rm -rf {SANDBOX_PROJECT_DIR}/build")
    run(sb, "ccache --zero-stats")
    start = time.perf_counter()
    run(sb, BUILD_CMD)
    elapsed = time.perf_counter() - start
    print(f"\nrebuild after rollback: {elapsed:.2f}s")

    print("\n=== ccache stats (expect cache hits > 0) ===")
    print(run(sb, "ccache --show-stats --verbose", timeout=60).stdout)
finally:
    sb.kill()
    if snapshot_id:
        Sandbox.delete_snapshot(snapshot_id)
        print(f"snapshot deleted: {snapshot_id}")
