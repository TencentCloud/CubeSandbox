#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pause and resume an OpenCode task in a CubeSandbox MicroVM."""

from __future__ import annotations

import shlex
import sys

from cubesandbox import Sandbox

from _opencode_common import (
    ensure_success,
    opencode_command,
    redacted_result_output,
    run_command,
    safe_kill,
    sandbox_identifier,
)
from env_utils import (
    RunConfig,
    build_opencode_env,
    load_local_dotenv,
    run_config,
)


TITLE_PREFIX = "cube-opencode-pause-resume"
SEED_FILES = {
    "README.md": "# OpenCode pause/resume example\n",
    "calculator.py": """def add(a: int, b: int) -> int:
    return a + b
""",
    "test_calculator.py": """import unittest

from calculator import add


class CalculatorTest(unittest.TestCase):
    def test_add(self) -> None:
        self.assertEqual(add(2, 3), 5)
""",
}
TURN_1_PROMPT = """Work only in {workspace}. Inspect the seeded Python project.
Create plan.md that says you will add multiply(a, b), test it, and record the
test result. Do not modify calculator.py or test_calculator.py yet."""
TURN_2_PROMPT = """Continue the task from plan.md. Implement multiply(a, b) in
calculator.py, add a unittest asserting multiply(4, 5) == 20, run
`python3 -m unittest discover -v`, and write result.md with the test result."""


def seed_project(sandbox: Sandbox, workspace: str, config: RunConfig) -> None:
    """Create the small project without persisting provider credentials."""
    result = run_command(
        sandbox,
        f"mkdir -p {shlex.quote(workspace)}",
        timeout=60,
    )
    ensure_success(
        result,
        "create the workspace",
        secrets=(config.provider.secret,),
    )
    for filename, content in SEED_FILES.items():
        path = f"{workspace.rstrip('/')}/{filename}"
        sandbox.files.write(path, content, user="root")


def print_result(result: object, config: RunConfig) -> None:
    """Print command output after removing any provider secret."""
    stdout, stderr = redacted_result_output(
        result,
        secrets=(config.provider.secret,),
    )
    if stdout:
        print(stdout, end="" if stdout.endswith("\n") else "\n")
    if stderr:
        print(stderr, file=sys.stderr, end="" if stderr.endswith("\n") else "\n")


def verify_workspace(sandbox: Sandbox, workspace: str, config: RunConfig) -> None:
    """Verify files from both turns are present after reconnecting."""
    command = (
        "test -f README.md && test -f plan.md && test -f calculator.py "
        "&& test -f test_calculator.py && test -f result.md "
        "&& grep -q 'def multiply' calculator.py "
        "&& python3 -m unittest discover -v"
    )
    result = run_command(sandbox, command, cwd=workspace, timeout=120)
    ensure_success(
        result,
        "verify workspace files survived pause/resume",
        secrets=(config.provider.secret,),
    )
    print_result(result, config)


def main() -> int:
    """Run an OpenCode turn, snapshot it, then resume and continue it."""
    load_local_dotenv()
    config = run_config()
    workspace = config.workspace
    command_env = build_opencode_env(config.provider, include_secret=True)
    sandbox: Sandbox | None = None

    try:
        sandbox = Sandbox.create(
            template=config.template_id,
            timeout=config.sandbox_timeout,
        )
        sandbox_id = sandbox_identifier(sandbox)
        title = f"{TITLE_PREFIX}-{sandbox_id}"
        print(f"Sandbox ready: {sandbox_id}")

        seed_project(sandbox, workspace, config)
        print(f"Seeded project in {workspace}")

        first_command = opencode_command(
            TURN_1_PROMPT.format(workspace=workspace),
            model=config.provider.model,
            workspace=workspace,
            title=title,
        )
        first_result = run_command(
            sandbox,
            first_command,
            cwd=workspace,
            envs=command_env,
            timeout=config.exec_timeout,
        )
        ensure_success(
            first_result,
            "run the first OpenCode turn",
            secrets=(config.provider.secret,),
        )
        print_result(first_result, config)

        saved_sandbox_id = sandbox_id
        pause_result = sandbox.pause()
        if isinstance(pause_result, str) and pause_result:
            sandbox_id = pause_result
        else:
            sandbox_id = saved_sandbox_id
        print(f"Paused sandbox; resume handle: {sandbox_id}")

        sandbox = Sandbox.connect(sandbox_id)
        print(f"Reconnected to sandbox: {sandbox_identifier(sandbox)}")

        second_command = opencode_command(
            TURN_2_PROMPT,
            model=config.provider.model,
            workspace=workspace,
            continue_last=True,
        )
        second_result = run_command(
            sandbox,
            second_command,
            cwd=workspace,
            envs=command_env,
            timeout=config.exec_timeout,
        )
        ensure_success(
            second_result,
            "run the resumed OpenCode turn",
            secrets=(config.provider.secret,),
        )
        print_result(second_result, config)

        verify_workspace(sandbox, workspace, config)
        print("Verified workspace persistence and completed OpenCode task.")
        return 0
    finally:
        safe_kill(sandbox)


if __name__ == "__main__":
    sys.exit(main())
