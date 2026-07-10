#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate CubeSandbox's snapshot, clone, and rollback capabilities.

CubeSandbox differentiators shown:
  - Snapshot outlives sandbox: snapshots persist independently — kill the
    source sandbox and still create new sandboxes from the checkpoint
  - Instant rollback: restore sandbox state in-memory without rebooting
  - Fast clone: fork N sandboxes from a single checkpoint via sb.clone(n=N)

Usage:
    python snapshot_rollback.py

Workflow:
    1. Create sandbox, set up a Rust project, build.
    2. Take snapshot A (checkpoint).
    3. Kill the source sandbox — snapshot A still exists independently.
    4. Clone from snapshot A into a new sandbox (fork).
    5. Rollback inside the clone to demonstrate instant restore.
"""

from __future__ import annotations

import sys
import time

from cubesandbox import Sandbox

from env_utils import load_local_dotenv, required

PROJECT_FILES: dict[str, str] = {
    "Cargo.toml": r'''[package]
name = "snapshot-demo"
version = "0.1.0"
edition = "2021"
''',
    "src/main.rs": r'''fn main() {
    println!("Checkpoint A: original version");
}
''',
}

NEW_MAIN_RS = r'''fn main() {
    println!("Checkpoint B: modified version with extra features");
    let sum: i32 = (1..=100).sum();
    println!("Sum of 1..100 = {}", sum);
}
'''

WS = "/home/user/workspace/snapshot-demo"


def run_cmd(sandbox: Sandbox, command: str, *, cwd: str | None = None,
            timeout: int = 60) -> str:
    result = sandbox.commands.run(command, cwd=cwd, timeout=timeout)
    if result.exit_code != 0:
        msg = f"Command failed (exit={result.exit_code}): {command}"
        if result.stderr:
            msg += f"\n  stderr: {result.stderr}"
        print(msg, file=sys.stderr)
        raise SystemExit(1)
    return (result.stdout or "")


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print("=" * 60)
    print("  CubeSandbox — Snapshot / Clone / Rollback Demo")
    print("=" * 60)

    # ── Phase 1: Create sandbox + build project ─────────────────────────
    print("\n[Phase 1] Creating sandbox and building project...")
    t0 = time.monotonic()

    sandbox = Sandbox.create(
        template=template_id,
        timeout=120,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
    )
    sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
    info = sandbox.get_info()
    print(f"  Created sandbox: {sandbox_id}  state={info.get('state', 'N/A')}"
          f"  ({time.monotonic() - t0:.2f}s)")

    run_cmd(sandbox, f"mkdir -p {WS}/src")
    for name, content in PROJECT_FILES.items():
        sandbox.files.write(f"{WS}/{name}", content)
    run_cmd(sandbox, "cargo build", cwd=WS, timeout=120)
    result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=WS)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A', got: {result!r}")
        return 1
    print(f"  ✓ Project built in {time.monotonic() - t0:.1f}s")

    # ── Phase 2: Take snapshot ──────────────────────────────────────────
    t0 = time.monotonic()
    snap = sandbox.create_snapshot(name="checkpoint-a")
    snap_id = snap.snapshot_id
    print(f"\n[Phase 2] Snapshot saved: {snap_id}  ({time.monotonic() - t0:.2f}s)")

    # Kill the source sandbox before cloning — snapshot is independent
    print("\n[Phase 2b] Killing source sandbox — snapshot still lives...")
    sandbox.kill()
    items, _ = Sandbox.list_snapshots(sandbox_id=sandbox_id)
    if not items:
        print("  FAIL: snapshot disappeared after sandbox kill!")
        return 1
    print(f"  ✓ Snapshot independent: {len(items)} snapshot(s) still in list"
          f"  ({time.monotonic() - t0:.2f}s)")

    # ── Phase 3: Clone from snapshot (fork) ─────────────────────────────
    t0 = time.monotonic()
    print(f"\n[Phase 3] Cloning from snapshot...")
    clone = Sandbox.create(template=snap_id, timeout=120)
    clone_id = getattr(clone, "sandbox_id", getattr(clone, "id", "unknown"))

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A' on clone, got: {result!r}")
        return 1
    print(f"  ✓ Clone ready: {clone_id}"
          f"  output={result.strip()!r}  ({time.monotonic() - t0:.2f}s)")

    # ── Phase 4: Modify clone ───────────────────────────────────────────
    print("\n[Phase 4] Modifying clone and rolling back...")
    clone.files.write(f"{WS}/src/main.rs", NEW_MAIN_RS)
    run_cmd(clone, "cargo build", cwd=WS, timeout=120)

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS)
    if "Checkpoint B" not in result:
        print(f"  FAIL: expected 'Checkpoint B', got: {result!r}")
        return 1

    # Rollback to checkpoint A
    t1 = time.monotonic()
    clone.rollback(snap_id)
    rollback_elapsed = time.monotonic() - t1
    print(f"  ✓ Rollback: {rollback_elapsed*1000:.0f}ms")

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A' after rollback, got: {result!r}")
        return 1
    print(f"  ✓ Verified: output={result.strip()!r}")

    # ── Phase 5: One-shot clone via sb.clone(n=N) ───────────────────────
    t0 = time.monotonic()
    print(f"\n[Phase 5] One-shot clone via sb.clone(n=3)...")
    clones = clone.clone(n=3)
    for i, sb in enumerate(clones):
        cid = getattr(sb, "sandbox_id", getattr(sb, "id", "unknown"))
        result = run_cmd(sb, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
        sb.kill()
        print(f"  clone {i+1}: {cid}  output={result.strip()!r}")
    print(f"  ✓ 3 clones in {time.monotonic() - t0:.2f}s")

    # ── Cleanup ─────────────────────────────────────────────────────────
    print(f"\n[Cleanup] Cleaning up...")
    Sandbox.delete_snapshot(snap_id)
    clone.kill()

    print(f"\n{'=' * 60}")
    print("  All snapshot / clone / rollback demos passed!")
    print(f"  Key takeaway: snapshots outlive the source sandbox."
          f"  Rollback in ~100ms.")
    print(f"{'=' * 60}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
