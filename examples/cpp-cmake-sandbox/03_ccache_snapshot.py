# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 03 (native cubesandbox SDK): the differentiated capability.
#
#   1. First build from scratch — ccache is cold, everything compiles.
#   2. create_snapshot() — captures the filesystem, INCLUDING the ccache
#      directory at /workspace/.ccache.
#   3. Spawn a fresh sandbox from that snapshot (template=snapshot_id).
#      It inherits the warm ccache.
#   4. Wipe the build dir and rebuild — ccache hits turn a cold recompile into
#      a near-instant incremental build.
#
# The script prints both timings and the speedup factor so the win is obvious.

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


def timed_build(sb) -> float:
    """Run a clean-config build and return wall-clock seconds."""
    start = time.perf_counter()
    run(sb, BUILD_CMD)
    return time.perf_counter() - start


snapshot_id = None
clone = None

sb = Sandbox.create(template=TEMPLATE_ID)
try:
    print(f"sandbox: {sb.sandbox_id}")
    push_project(sb)

    # 1) Cold build — populate the compile cache.
    run(sb, "ccache --zero-stats")
    first_build = timed_build(sb)
    print(f"\n[cold]  first build: {first_build:.2f}s")
    print(run(sb, "ccache --show-stats --verbose", timeout=60).stdout)

    # 2) Snapshot — the ccache under /workspace/.ccache travels with it.
    snapshot = sb.create_snapshot()
    snapshot_id = snapshot.snapshot_id
    print(f"snapshot created: {snapshot_id}")
finally:
    sb.kill()

# 3) Fresh sandbox from the snapshot — inherits the warm ccache.
clone = Sandbox.create(template=snapshot_id)
try:
    print(f"\nclone from snapshot: {clone.sandbox_id}")

    # 4) Force a full recompile: drop build artifacts, keep the ccache.
    run(clone, f"rm -rf {SANDBOX_PROJECT_DIR}/build")
    run(clone, "ccache --zero-stats")
    second_build = timed_build(clone)
    print(f"\n[warm]  incremental build after snapshot: {second_build:.2f}s")
    print(run(clone, "ccache --show-stats --verbose", timeout=60).stdout)

    speedup = first_build / second_build if second_build > 0 else float("inf")
    print("=" * 48)
    print(f"first build (cold ccache):   {first_build:6.2f}s")
    print(f"rebuild after snapshot:      {second_build:6.2f}s")
    print(f"speedup:                     {speedup:6.1f}x")
    print("=" * 48)
finally:
    clone.kill()
    if snapshot_id:
        Sandbox.delete_snapshot(snapshot_id)
        print(f"snapshot deleted: {snapshot_id}")
