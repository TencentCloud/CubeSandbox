#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run one reproducible OpenCode repair task inside CubeSandbox."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from _opencode_common import ensure_success, run_command, sandbox_identifier
from e2b import Sandbox
from env_utils import (
    build_opencode_env,
    int_env,
    load_local_dotenv,
    opencode_command,
    opencode_workspace,
    required,
    shell_join,
)

DEFAULT_PROMPT = """\
Inspect stats.py and tests/test_stats.py in {workspace}.
Run the target test first, explain the root cause, and make the smallest fix in
stats.py. Do not modify the test or add dependencies. Run the same test again,
then write a concise evidence summary to {workspace}/result.md.
"""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a one-shot OpenCode repair task inside CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="Cube template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--workspace",
        default=opencode_workspace(),
        help="Workspace inside the MicroVM.",
    )
    parser.add_argument("--prompt", default=None, help="Override the repair prompt.")
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("OPENCODE_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("OPENCODE_EXEC_TIMEOUT", 900),
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="Use an existing project in the workspace.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream raw OpenCode JSONL events.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def seed_project(sandbox: Sandbox, workspace: str) -> None:
    quoted = shlex.quote(workspace)
    command = f"""set -eu
mkdir -p {quoted}/tests
cat > {quoted}/stats.py <<'PY'
def mean(values: list[float]) -> float:
    if not values:
        raise ValueError("mean requires at least one value")
    return sum(values) // len(values)
PY
cat > {quoted}/tests/test_stats.py <<'PY'
import unittest

from stats import mean


class MeanTests(unittest.TestCase):
    def test_preserves_fractional_result(self) -> None:
        self.assertEqual(mean([1.0, 2.0]), 1.5)

    def test_rejects_empty_input(self) -> None:
        with self.assertRaises(ValueError):
            mean([])


if __name__ == "__main__":
    unittest.main()
PY
cat > {quoted}/README.md <<'MD'
# OpenCode repair fixture

This deterministic fixture contains one implementation bug. The test is the
behavior contract and must not be changed.
MD
cd {quoted}
git init -q
git add README.md stats.py tests/test_stats.py
git -c user.name=CubeDemo -c user.email=cube@example.invalid \
  commit -q -m baseline
"""
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "seed the deterministic repair project")


def verify_result(sandbox: Sandbox, workspace: str) -> None:
    quoted = shlex.quote(workspace)
    command = shell_join(
        f"cd {quoted}",
        "git diff --exit-code -- tests/test_stats.py",
        "python3 -m unittest -q tests/test_stats.py",
        "git diff --check",
        "git diff --stat",
        "git diff -- stats.py",
        "test -s result.md",
        "printf '\\n--- result.md ---\\n'",
        "cat result.md",
    )
    result = run_command(sandbox, command, timeout=120)
    ensure_success(result, "verify OpenCode's repair and evidence")
    stdout = getattr(result, "stdout", "")
    if stdout:
        print(stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["OPENCODE_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    opencode_env = build_opencode_env(include_secret=True)
    command = opencode_command(args.prompt)

    print(f"Creating sandbox from template: {template_id}")
    result = None
    # This simple flavor injects HY3_API_KEY only into the OpenCode process but
    # leaves internet egress open. A compromised task could still exfiltrate it;
    # shared clusters should use network_policy.py instead.
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version = run_command(sandbox, "opencode --version", timeout=60)
        ensure_success(version, "check OpenCode version")
        print(f"OpenCode version: {getattr(version, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace)
            print(f"Seeded repair fixture in {args.workspace}")

        print("\nRunning OpenCode with Hy3...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=opencode_env,
            timeout=args.exec_timeout,
            stream=True,
        )
        ensure_success(result, "run OpenCode")
        verify_result(sandbox, args.workspace)

    exit_code = getattr(result, "exit_code", 1)
    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
