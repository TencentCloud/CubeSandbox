#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Multi-sandbox collaboration with role-based network isolation.

CubeSandbox differentiators shown:
  - Role-based isolation: builder sandbox has internet, runner sandbox is air-gapped
  - Cross-sandbox artifact transfer: host reads binary from builder,
    writes it into runner
  - Per-sandbox egress policy: each sandbox gets its own allow_internet_access
  - Lifecycle: auto-pause/resume via lifecycle config

Usage:
    python multi_container.py

Workflow:
    1. Create a builder sandbox (internet allowed) — downloads crates, compiles.
    2. Read the compiled binary from the builder via host-side SDK.
    3. Create a runner sandbox (air-gapped) — write the binary, execute it.
    4. Runner succeeds without ever touching the internet.
"""

from __future__ import annotations

import sys
import time

from e2b import Sandbox

from env_utils import load_local_dotenv, required

BUILDER_CARGO_TOML = r'''[package]
name = "multi-container-demo"
version = "0.1.0"
edition = "2021"

[dependencies]
serde_json = "1"
chrono = "0.4"
'''

BUILDER_MAIN_RS = r'''use chrono::Utc;
use serde_json::json;

fn main() {
    let now = Utc::now();
    let message = json!({
        "service": "builder",
        "status": "compiled",
        "timestamp": now.to_rfc3339(),
        "version": env!("CARGO_PKG_VERSION"),
    });
    println!("{}", serde_json::to_string_pretty(&message).unwrap());
}
'''

BUILDER_WS = "/home/user/workspace/multi-container-demo"


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print("=" * 60)
    print("  CubeSandbox — Multi-Sandbox Collaboration Demo")
    print("=" * 60)

    # ── Step 1: Builder sandbox (with internet) ─────────────────────────
    print("\n[Step 1] Creating builder sandbox (internet allowed)...")
    t0 = time.monotonic()

    with Sandbox.create(
        template=template_id,
        timeout=120,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        allow_internet_access=True,
    ) as builder:
        builder_id = builder.sandbox_id
        info = builder.get_info()
        print(f"  Builder: {builder_id}  state={info.state.value}"
              f"  ({time.monotonic() - t0:.2f}s)")

        # Scaffold project with dependencies
        builder.commands.run(f"mkdir -p {BUILDER_WS}/src", timeout=30)
        builder.files.write(f"{BUILDER_WS}/Cargo.toml", BUILDER_CARGO_TOML)
        builder.files.write(f"{BUILDER_WS}/src/main.rs", BUILDER_MAIN_RS)

        print("  Builder: downloading crates and compiling...")
        t1 = time.monotonic()
        result = builder.commands.run("cargo build --release", cwd=BUILDER_WS, timeout=300)

        if result.exit_code != 0:
            print(f"  Builder: build FAILED — {result.stderr[-300:]}", file=sys.stderr)
            return 1

        print(f"  Builder: compile succeeded in {time.monotonic() - t1:.1f}s")

        # Read binary from builder via host SDK (format="bytes" preserves ELF)
        raw_data = builder.files.read(f"{BUILDER_WS}/target/release/multi-container-demo",
                                      format="bytes")
        binary_data = bytes(raw_data)
        if not binary_data:
            print("  Builder: binary not found!", file=sys.stderr)
            return 1
        print(f"  Builder: binary read ({len(binary_data)} bytes)")

    # Builder sandbox is now killed (context manager exit)
    print("  Builder: sandbox terminated (context manager exit)")

    # ── Step 2: Runner sandbox (air-gapped) ─────────────────────────────
    print("\n[Step 2] Creating runner sandbox (air-gapped)...")
    t0 = time.monotonic()

    with Sandbox.create(
        template=template_id,
        timeout=60,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        allow_internet_access=False,
    ) as runner:
        runner_id = runner.sandbox_id
        info = runner.get_info()
        print(f"  Runner: {runner_id}  state={info.state.value}"
              f"  ({time.monotonic() - t0:.2f}s)")

        # Write binary from host into runner
        runner_dir = "/home/user/workspace/runner"
        runner.commands.run(f"mkdir -p {runner_dir}", timeout=30)
        runner.files.write(f"{runner_dir}/multi-container-demo", binary_data)
        runner.commands.run(f"chmod +x {runner_dir}/multi-container-demo", timeout=30)
        print("  Runner: binary transferred from builder")

        # Execute binary — succeeds without internet access
        result = runner.commands.run(f"{runner_dir}/multi-container-demo", timeout=30)
        if result.exit_code != 0:
            print(f"  Runner: execution FAILED — {result.stderr[-300:]}", file=sys.stderr)
            return 1

        output = (result.stdout or "").strip()
        print(f"  Runner: output={output}")

    print(f"\n{'=' * 60}")
    print("  Multi-sandbox collaboration demo passed!")
    print("  Key takeaway: builder downloads dependencies, runner is air-gapped.")
    print("  Cross-sandbox artifact transfer via host SDK.")
    print(f"{'=' * 60}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
