#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Build a CMake project, pause it, then resume with ccache preserved."""

from __future__ import annotations

import os
import sys

from e2b import Sandbox

from env_utils import load_local_dotenv, required

WORKSPACE = "/workspace/cpp-cmake-demo"

FIRST_BUILD = f"""\
set -eu
rm -rf {WORKSPACE}
mkdir -p {WORKSPACE}
cp -a /opt/cpp-cmake-sandbox/sample/. {WORKSPACE}/
cd {WORKSPACE}
ccache --zero-stats || true
cmake -S . -B build -G Ninja -DCMAKE_CXX_COMPILER_LAUNCHER=ccache
cmake --build build
ctest --test-dir build --output-on-failure
./build/cpp-cmake-demo CubeSandbox
printf 'first build completed\\n' > .snapshot-marker
printf '\\n--- ccache after first build ---\\n'
ccache --show-stats
"""

RESUMED_BUILD = f"""\
set -eu
cd {WORKSPACE}
test -f .snapshot-marker
touch src/greeting.cpp
cmake --build build
ctest --test-dir build --output-on-failure
./build/cpp-cmake-demo resumed
printf '\\n--- ccache after resumed build ---\\n'
ccache --show-stats
"""


def run_checked(sandbox: Sandbox, command: str, label: str) -> str:
    result = sandbox.commands.run(command, timeout=300)
    stdout = getattr(result, "stdout", "")
    stderr = getattr(result, "stderr", "")
    if stdout:
        print(stdout, end="" if stdout.endswith("\n") else "\n")
    if getattr(result, "exit_code", 0) != 0:
        raise RuntimeError(f"{label} failed (exit {result.exit_code}): {stderr}")
    return stdout


def main() -> int:
    load_local_dotenv()
    required("E2B_API_URL")
    required("E2B_API_KEY")
    template_id = os.environ.get("CUBE_TEMPLATE_ID") or required("CUBE_TEMPLATE_ID")

    sandbox = Sandbox.create(template=template_id, timeout=1800)
    sandbox_id = sandbox.sandbox_id
    try:
        print(f"Sandbox ready: {sandbox_id}")
        print("\n=== First build ===")
        first_output = run_checked(sandbox, FIRST_BUILD, "first build")
        if "Hello, CubeSandbox!" not in first_output:
            raise RuntimeError("the first build did not run the expected binary")

        print("\n=== Pause sandbox (snapshot VM state) ===")
        sandbox.pause()

        print("\n=== Resume sandbox and rebuild ===")
        sandbox = Sandbox.connect(sandbox_id)
        resumed_output = run_checked(sandbox, RESUMED_BUILD, "resumed build")
        if "Hello, resumed!" not in resumed_output:
            raise RuntimeError("the resumed build did not run the expected binary")

        print("\nSuccess: CMake build directory and ccache survived pause/resume.")
        return 0
    finally:
        sandbox.kill()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
