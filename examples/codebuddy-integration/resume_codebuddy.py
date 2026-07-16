#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate CodeBuddy coding-agent session persistence across a CubeSandbox pause/resume.

Turn 1 asks CodeBuddy to write ``/workspace/plan.md``, then the sandbox is paused.
Turn 2 reconnects to the same sandbox, verifies both ``/workspace`` and the
CodeBuddy state directory survived the snapshot, and asks CodeBuddy to continue the work.

Lifecycle note: this script deliberately avoids ``with Sandbox.create(...)``.
A context manager kills the sandbox on ``__exit__``, which would defeat the
pause. The lifecycle is managed manually with try/finally so the sandbox stays
alive between turns and is only killed at the very end.
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from env_utils import (
    build_codebuddy_env,
    codebuddy_command,
    codebuddy_provider,
    codebuddy_workspace,
    int_env,
    load_local_dotenv,
    require_provider_key,
    required,
    shell_join,
)
from _common import ensure_success, run_command, sandbox_identifier

DEFAULT_CODEBUDDY_STATE_DIR = "/root/.codebuddy"

TURN_1_PROMPT = (
    "Create {workspace}/plan.md containing a numbered 3-step plan for building a "
    "small Python CLI that prints the current time. Only write the plan file."
)
TURN_2_PROMPT = (
    "Read {workspace}/plan.md and implement step 1 by creating "
    "{workspace}/progress.md that records which step you completed and why. "
    "Do not delete plan.md."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Demonstrate CodeBuddy session persistence across a CubeSandbox pause/resume."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--workspace",
        default=codebuddy_workspace(),
        help="Working directory inside the sandbox. Defaults to CODEBUDDY_WORKSPACE.",
    )
    parser.add_argument(
        "--name",
        default="cube-codebuddy-resume",
        help="CodeBuddy session name reused across both turns to keep the conversation.",
    )
    parser.add_argument(
        "--codebuddy-state-dir",
        default=os.environ.get("CODEBUDDY_HOME", DEFAULT_CODEBUDDY_STATE_DIR),
        help="CodeBuddy state directory checked for survival after resume.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("CODEBUDDY_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to CODEBUDDY_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CODEBUDDY_EXEC_TIMEOUT", 900),
        help="CodeBuddy command timeout in seconds. Defaults to CODEBUDDY_EXEC_TIMEOUT or 900.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream CodeBuddy's raw JSONL instead of the concise transcript.",
    )
    return parser.parse_args()


def run_turn(
    sandbox: Sandbox,
    workspace: str,
    prompt: str,
    name: str,
    exec_timeout: int,
    envs: dict[str, str],
    raw: bool = False,
):
    command = shell_join(
        f"cd {shlex.quote(workspace)}",
        codebuddy_command(prompt, output_format="stream-json", name=name),
    )
    return run_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=exec_timeout,
        stream=True,
        raw=raw,
    )


def assert_state_survived(sandbox: Sandbox, workspace: str, state_dir: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    quoted_state = shlex.quote(state_dir)
    command = shell_join(
        f"test -f {quoted_workspace}/plan.md",
        f"test -d {quoted_state}",
        "printf '\\n--- plan.md (survived pause/resume) ---\\n'",
        f"cat {quoted_workspace}/plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify /workspace and CodeBuddy state survived pause/resume")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def show_final_workspace(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted_workspace}",
        f"test ! -f {quoted_workspace}/progress.md || "
        f"(printf '\\n--- progress.md ---\\n' && cat {quoted_workspace}/progress.md)",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "inspect final workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_provider_key(codebuddy_provider())

    codebuddy_env = build_codebuddy_env()
    turn_1_prompt = TURN_1_PROMPT.format(workspace=args.workspace)
    turn_2_prompt = TURN_2_PROMPT.format(workspace=args.workspace)

    print(f"Creating sandbox from template: {template_id}")
    # SECURITY: like run_codebuddy.py this demo keeps egress open and injects the
    # key per command. The pause() snapshot also captures the in-VM env and any
    # credentials CodeBuddy caches under /root/.codebuddy, widening exposure — for
    # shared clusters prefer the default-deny + vault pattern in network_policy.py.
    sandbox = None
    sandbox_id = "uncreated"
    try:
        sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "codebuddy --version", timeout=60)
        ensure_success(version_result, "check CodeBuddy version")
        print(f"CodeBuddy version: {getattr(version_result, 'stdout', '').strip()}")

        print("\n=== Turn 1: create plan.md ===\n")
        result_1 = run_turn(
            sandbox,
            args.workspace,
            turn_1_prompt,
            args.name,
            args.exec_timeout,
            codebuddy_env,
            args.raw,
        )
        ensure_success(result_1, "run CodeBuddy turn 1")

        print(f"\nPausing sandbox {sandbox_id} (snapshotting VM + rootfs)...")
        paused_id = sandbox.pause()
        # The sandbox_id is stable across pause. Some SDK versions return the
        # resume handle as a string; others return a bool (success). Only adopt
        # a string handle, otherwise keep the original id for connect().
        if isinstance(paused_id, str) and paused_id:
            sandbox_id = paused_id
        print(f"Paused. Resume handle: {sandbox_id}")

        print(f"\nReconnecting to {sandbox_id}...")
        sandbox = Sandbox.connect(sandbox_id=sandbox_id, timeout=args.sandbox_timeout)
        print("Reconnected after resume.")

        print("\n=== Verifying persistence after resume ===\n")
        assert_state_survived(sandbox, args.workspace, args.codebuddy_state_dir)

        print("\n=== Turn 2: continue the work ===\n")
        result_2 = run_turn(
            sandbox,
            args.workspace,
            turn_2_prompt,
            args.name,
            args.exec_timeout,
            codebuddy_env,
            args.raw,
        )
        ensure_success(result_2, "run CodeBuddy turn 2")

        show_final_workspace(sandbox, args.workspace)

        exit_code = getattr(result_2, "exit_code", 0)
        return 0 if exit_code is None else int(exit_code)
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
                print(f"\nSandbox {sandbox_id} killed.")
            except Exception as exc:  # noqa: BLE001 - cleanup must not mask real errors
                print(
                    f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    sys.exit(main())
