#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Minimal Rust playground: write, compile, and run a Rust program inside a sandbox.

Usage:
    python hello_world.py

This script:
    1. Creates a sandbox from a Rust-enabled template.
    2. Writes a Rust source file into the sandbox.
    3. Compiles it with ``rustc``.
    4. Runs the compiled binary.
"""

from __future__ import annotations

import sys

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


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print(f"Creating sandbox from template: {template_id}")

    with Sandbox.create(
        template=template_id,
        timeout=120,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
    ) as sandbox:
        sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))

        info = sandbox.get_info()
        print(f"Sandbox ready: {sandbox_id}  state={info.get('state', 'N/A')}")

        # 1. Write the Rust source file
        print("\n--- Writing hello.rs ---")
        sandbox.files.write("/home/user/workspace/hello.rs", HELLO_RS)
        print("hello.rs written.")

        # 2. Compile
        print("\n--- Compiling with rustc ---")
        result = sandbox.commands.run(
            "rustc hello.rs",
            cwd="/home/user/workspace",
            timeout=120,
        )
        print(f"rustc exit code: {result.exit_code}")
        if result.stderr:
            print("stderr:", result.stderr)
        if result.stdout:
            print("stdout:", result.stdout)

        if result.exit_code != 0:
            print("Compilation failed!", file=sys.stderr)
            return 1

        # 3. Run the binary
        print("\n--- Running hello ---")
        result = sandbox.commands.run(
            "./hello",
            cwd="/home/user/workspace",
            timeout=30,
        )
        print(f"exit code: {result.exit_code}")
        if result.stdout:
            print("stdout:", result.stdout)
        if result.stderr:
            print("stderr:", result.stderr)

        print("\nRust playground demo completed successfully!")

    return 0


if __name__ == "__main__":
    sys.exit(main())
