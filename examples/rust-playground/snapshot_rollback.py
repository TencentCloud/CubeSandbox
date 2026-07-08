#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate CubeCoW snapshot, clone, and rollback with a Rust project.

This showcases CubeSandbox's most differentiated capability: hundred-ms
checkpoints on running sandboxes with the ability to roll back or fork
from any saved state — perfect for iterative development and experimentation.

Workflow:
    1. Create a sandbox with a Rust project checked out.
    2. Take a snapshot (checkpoint A).
    3. Make changes (edits, experiments) — checkpoint B.
    4. Roll back to checkpoint A and verify the original state is restored.
    5. Clone from checkpoint A to create a fork and verify isolation.

Usage:
    python snapshot_rollback.py
"""

from __future__ import annotations

import sys

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

    ws = "/home/user/workspace/snapshot-demo"

    print("=" * 60)
    print("CubeSandbox Snapshot / Clone / Rollback Demo")
    print("=" * 60)

    # --- Phase 1: Create sandbox with a Rust project ---
    print("\n[Phase 1] Creating sandbox and setting up project...")
    sandbox = Sandbox.create(template=template_id, timeout=120)
    sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
    print(f"  Sandbox ID: {sandbox_id}")

    clone = None

    try:
        run_cmd(sandbox, f"mkdir -p {ws}/src")
        for name, content in PROJECT_FILES.items():
            sandbox.files.write(f"{ws}/{name}", content)
        run_cmd(sandbox, "cargo build", cwd=ws, timeout=120)
        result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=ws)
        print(f"  Output: {result.strip()}")
        if "Checkpoint A" not in result:
            print(f"FAIL: Expected 'Checkpoint A', got: {result!r}", file=sys.stderr)
            return 1
        print("  ✓ Project built and verified.")

        # --- Phase 2: Take snapshot A ---
        print("\n[Phase 2] Taking snapshot (checkpoint A)...")
        snap_info = sandbox.create_snapshot(name="checkpoint-a")
        snap_id = snap_info.snapshot_id
        print(f"  ✓ Snapshot saved. ID: {snap_id}")

        # --- Phase 3: Modify the project (simulate iterative dev) ---
        print("\n[Phase 3] Modifying project (checkpoint B)...")
        sandbox.files.write(f"{ws}/src/main.rs", NEW_MAIN_RS)
        run_cmd(sandbox, "cargo build", cwd=ws, timeout=120)
        result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=ws)
        print(f"  Output: {result.strip()}")
        if "Checkpoint B" not in result:
            print(f"FAIL: Expected 'Checkpoint B', got: {result!r}", file=sys.stderr)
            return 1
        print("  ✓ Modified version running with new behavior.")

        # --- Phase 4: Rollback to checkpoint A ---
        print("\n[Phase 4] Rolling back to checkpoint A...")
        sandbox.rollback(snap_id)
        print("  ✓ Rollback completed. Sandbox is back at checkpoint A state.")

        result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=ws, timeout=30)
        print(f"  Output after rollback: {result.strip()}")
        if "Checkpoint A" not in result:
            print(f"FAIL: Expected 'Checkpoint A' after rollback, got: {result!r}", file=sys.stderr)
            return 1
        print("  ✓ Rollback verified: original version restored.")

        # --- Phase 5: Clone from checkpoint A ---
        print("\n[Phase 5] Cloning from checkpoint A to create an isolated fork...")
        clone = Sandbox.create(template=snap_id, timeout=120)
        clone_id = getattr(clone, "sandbox_id", getattr(clone, "id", "unknown"))
        print(f"  Clone sandbox ID: {clone_id}")

        result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=ws, timeout=30)
        print(f"  Clone output: {result.strip()}")
        if "Checkpoint A" not in result:
            print(f"FAIL: Expected 'Checkpoint A' on clone, got: {result!r}", file=sys.stderr)
            return 1
        print("  ✓ Clone is at checkpoint A, isolated from original.")

        # Modify clone independently
        print("\n[Phase 6] Modifying clone independently...")
        clone.files.write(f"{ws}/src/main.rs", NEW_MAIN_RS.replace('"Checkpoint B',
                                                                    '"Clone fork'))
        run_cmd(clone, "cargo build", cwd=ws, timeout=120)
        result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=ws)
        print(f"  Clone output after fork: {result.strip()}")
        if "Clone fork" not in result:
            print(f"FAIL: Expected 'Clone fork', got: {result!r}", file=sys.stderr)
            return 1

        # Verify original sandbox is unaffected
        result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=ws, timeout=30)
        print(f"  Original sandbox output: {result.strip()}")
        if "Checkpoint A" not in result:
            print(f"FAIL: Expected 'Checkpoint A' on original, got: {result!r}", file=sys.stderr)
            return 1
        print("  ✓ Fork isolation confirmed: original and clone diverged independently.")

    finally:
        print("\n[Cleanup] Killing sandboxes...")
        sandbox.kill()
        if clone is not None:
            clone.kill()
        print("  ✓ Both sandboxes killed.")

    print("\n" + "=" * 60)
    print("All snapshot / clone / rollback demos passed!")
    print("=" * 60)
    return 0


if __name__ == "__main__":
    sys.exit(main())
