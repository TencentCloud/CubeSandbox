#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run one deterministic OpenCode task inside a CubeSandbox MicroVM."""

from __future__ import annotations

import argparse
import shlex
import sys

from cubesandbox import Sandbox

from _opencode_common import (
    ensure_success,
    extract_session_id,
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


SUCCESS_MARKER = "OPENCODE_CUBE_OK"
DEFAULT_TITLE = "cube-opencode-one-shot"
DEFAULT_PROMPT = (
    "Inspect the Python project in {workspace}. Implement multiply(a, b) in "
    "calculator.py, add a unittest asserting multiply(4, 5) == 20, run "
    "`python3 -m unittest discover -v`, and write result.md containing the "
    f"exact marker {SUCCESS_MARKER}."
)

SEED_FILES = {
    "README.md": """# CubeSandbox OpenCode Smoke Project

This deterministic project verifies that OpenCode can edit files and run tests
inside an isolated CubeSandbox MicroVM.
""",
    "calculator.py": """def add(a: int, b: int) -> int:
    return a + b
""",
    "test_calculator.py": """import unittest

from calculator import add


class CalculatorTest(unittest.TestCase):
    def test_adds_two_numbers(self) -> None:
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
""",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a one-shot OpenCode task inside CubeSandbox."
    )
    parser.add_argument(
        "--prompt",
        help="Prompt passed to OpenCode. Defaults to the deterministic smoke task.",
    )
    parser.add_argument(
        "--workspace",
        help="Workspace inside the sandbox. Defaults to OPENCODE_WORKSPACE.",
    )
    parser.add_argument(
        "--title",
        help="OpenCode session title. Defaults to a sandbox-specific title.",
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="Do not create the deterministic calculator project.",
    )
    return parser.parse_args()


def seed_project(sandbox: Sandbox, workspace: str, config: RunConfig) -> None:
    mkdir = run_command(
        sandbox,
        f"mkdir -p {shlex.quote(workspace)}",
        timeout=60,
    )
    ensure_success(mkdir, "create the workspace", secrets=(config.provider.secret,))

    for filename, content in SEED_FILES.items():
        path = f"{workspace.rstrip('/')}/{filename}"
        sandbox.files.write(path, content, user="root")


def verify_result(sandbox: Sandbox, workspace: str, config: RunConfig) -> None:
    marker = shlex.quote(SUCCESS_MARKER)
    command = (
        "python3 -m unittest discover -v "
        "&& grep -q 'def multiply' calculator.py "
        "&& test -f result.md "
        f"&& grep -q {marker} result.md"
    )
    result = run_command(
        sandbox,
        command,
        cwd=workspace,
        timeout=120,
    )
    ensure_success(
        result,
        "verify OpenCode's workspace changes",
        secrets=(config.provider.secret,),
    )


def resolve_session_id(
    sandbox: Sandbox,
    output: str,
    title: str,
    workspace: str,
    command_env: dict[str, str],
    config: RunConfig,
) -> str:
    try:
        return extract_session_id(output)
    except ValueError:
        listing = run_command(
            sandbox,
            "opencode session list --format json",
            cwd=workspace,
            envs=command_env,
            timeout=60,
        )
        ensure_success(
            listing,
            "list OpenCode sessions",
            secrets=(config.provider.secret,),
        )
        return extract_session_id(listing.stdout, title=title)


def print_redacted_result(result: object, config: RunConfig) -> None:
    stdout, stderr = redacted_result_output(
        result,
        secrets=(config.provider.secret,),
    )
    if stdout:
        print(stdout, end="" if stdout.endswith("\n") else "\n")
    if stderr:
        print(stderr, file=sys.stderr, end="" if stderr.endswith("\n") else "\n")


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    config = run_config()
    workspace = args.workspace or config.workspace
    command_env = build_opencode_env(config.provider, include_secret=True)

    sandbox: Sandbox | None = None
    try:
        print(f"Creating sandbox from template: {config.template_id}")
        print(
            "Security note: this one-shot example passes the provider key into "
            "the VM. Use network_policy.py for shared or production clusters.",
            file=sys.stderr,
        )
        sandbox = Sandbox.create(
            template=config.template_id,
            timeout=config.sandbox_timeout,
        )
        sandbox_id = sandbox_identifier(sandbox)
        title = args.title or f"{DEFAULT_TITLE}-{sandbox_id}"
        print(f"Sandbox ready: {sandbox_id}")

        version = run_command(sandbox, "opencode --version", timeout=60)
        ensure_success(
            version,
            "check the OpenCode version",
            secrets=(config.provider.secret,),
        )
        print(f"OpenCode version: {version.stdout.strip()}")

        if not args.no_seed:
            seed_project(sandbox, workspace, config)
            print(f"Seeded deterministic project in {workspace}")

        prompt = args.prompt or DEFAULT_PROMPT.format(workspace=workspace)
        command = opencode_command(
            prompt,
            model=config.provider.model,
            workspace=workspace,
            title=title,
        )
        print("\nRunning OpenCode task...\n")
        result = run_command(
            sandbox,
            command,
            cwd=workspace,
            envs=command_env,
            timeout=config.exec_timeout,
        )
        ensure_success(
            result,
            "run the OpenCode task",
            secrets=(config.provider.secret,),
        )
        print_redacted_result(result, config)

        verify_result(sandbox, workspace, config)
        session_id = resolve_session_id(
            sandbox,
            result.stdout,
            title,
            workspace,
            command_env,
            config,
        )
        print(f"\nVerified marker {SUCCESS_MARKER}; session: {session_id}")
        return 0
    finally:
        safe_kill(sandbox)


if __name__ == "__main__":
    sys.exit(main())
