#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Rust project with Cargo dependencies: build and run a Cargo project inside a sandbox.

Usage:
    python with_dependencies.py

This script demonstrates:
    1. Creating a sandbox from a Rust-enabled template.
    2. Scaffolding a Cargo project with external crates (serde_json, chrono).
    3. Building the project with ``cargo build``.
    4. Running the resulting binary.
"""

from __future__ import annotations

import sys

from e2b import Sandbox

from env_utils import load_local_dotenv, required

CARGO_TOML = r'''[package]
name = "sandbox-demo"
version = "0.1.0"
edition = "2021"

[dependencies]
serde_json = "1"
chrono = "0.4"
'''

MAIN_RS = r'''use chrono::Utc;
use serde_json::json;

fn main() {
    let now = Utc::now();
    let message = json!({
        "greeting": "Hello from CubeSandbox!",
        "language": "Rust",
        "timestamp": now.to_rfc3339(),
        "crates": ["serde_json", "chrono"],
        "answer": 42,
    });
    println!("{}", serde_json::to_string_pretty(&message).unwrap());
}
'''


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print(f"Creating sandbox from template: {template_id}")

    with Sandbox.create(template=template_id, timeout=360) as sandbox:
        sandbox_id = getattr(sandbox, "sandbox_id", None) or sandbox.id
        print(f"Sandbox ready: {sandbox_id}")

        ws = "/home/user/workspace/sandbox-demo"

        # 1. Scaffold project
        print("\n--- Creating Cargo project ---")
        sandbox.commands.run(f"mkdir -p {ws}/src", timeout=30)
        sandbox.files.write(f"{ws}/Cargo.toml", CARGO_TOML)
        sandbox.files.write(f"{ws}/src/main.rs", MAIN_RS)

        print("Project scaffolded.")

        # 2. Build
        print("\n--- cargo build (fetching dependencies + compiling) ---")
        result = sandbox.commands.run(
            "cargo build --release",
            cwd=ws,
            timeout=300,
        )
        print(f"cargo build exit code: {result.exit_code}")
        if result.stdout:
            tail = result.stdout[-2000:] if len(result.stdout) > 2000 else result.stdout
            print(tail)
        if result.stderr:
            tail = result.stderr[-2000:] if len(result.stderr) > 2000 else result.stderr
            print("stderr:", tail)

        if result.exit_code != 0:
            print("Cargo build failed!", file=sys.stderr)
            return 1

        # 3. Run
        print("\n--- Running sandbox-demo ---")
        result = sandbox.commands.run(
            "./target/release/sandbox-demo",
            cwd=ws,
            timeout=30,
        )
        if result.stdout:
            print("stdout:", result.stdout)
        if result.stderr:
            print("stderr:", result.stderr)

        print("\nCargo project demo completed successfully!")

    return 0


if __name__ == "__main__":
    sys.exit(main())
