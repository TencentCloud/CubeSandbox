#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate Claude Code session persistence across a CubeSandbox pause/resume.

Turn 1 asks Claude Code to write ``/workspace/plan.md``, then the sandbox is
paused. Turn 2 reconnects to the same sandbox, verifies both ``/workspace`` and
Claude Code's state directory survived the snapshot, and asks Claude Code to
continue the work.

Lifecycle note: this script deliberately avoids ``with Sandbox.create(...)``.
A context manager kills the sandbox on ``__exit__``, which would defeat the
pause. The lifecycle is managed manually with try/finally so the sandbox stays
alive between turns and is only killed at the very end.

Usage:
    python resume_claude_code.py
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from common import (
    build_cc_env,
    cc_command,
    cc_model,
    cc_workspace,
    ensure_success,
    int_env,
    load_dotenv,
    optional,
    require_api_key,
    required,
    run_command,
    sandbox_identifier,
    shell_join,
)

DEFAULT_CC_STATE_DIR = "/home/user/.claude"

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
        description="Demonstrate Claude Code session persistence across pause/resume."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--workspace",
        default=cc_workspace(),
        help="Working directory inside the sandbox. Defaults to CC_WORKSPACE.",
    )
    parser.add_argument(
        "--model",
        default=cc_model(),
        help="Model for Claude Code. Defaults to CC_MODEL or claude-sonnet-4-6.",
    )
    parser.add_argument(
        "--effort",
        default=optional("CC_EFFORT"),
        help="Effort level: low, medium, high, xhigh, max.",
    )
    parser.add_argument(
        "--cc-state-dir",
        default=os.environ.get("CLAUDE_CODE_STATE_DIR", DEFAULT_CC_STATE_DIR),
        help="Claude Code state directory checked for survival after resume.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("CC_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to CC_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CC_EXEC_TIMEOUT", 900),
        help="Claude Code command timeout in seconds.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream Claude Code's raw JSON instead of the concise transcript.",
    )
    return parser.parse_args()


def run_turn(
    sandbox: Sandbox,
    workspace: str,
    prompt: str,
    model: str,
    effort: str | None,
    exec_timeout: int,
    envs: dict[str, str],
):
    command = shell_join(
        f"cd {shlex.quote(workspace)}",
        cc_command(
            prompt,
            model=model,
            effort=effort,
            dangerously_skip_permissions=True,
        ),
    )
    return run_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=exec_timeout,
        stream=True,
        user="user",
    )


def assert_state_survived(sandbox: Sandbox, workspace: str, state_dir: str) -> None:
    quoted_ws = shlex.quote(workspace)
    quoted_state = shlex.quote(state_dir)
    command = shell_join(
        f"test -f {quoted_ws}/plan.md",
        f"test -d {quoted_state}",
        "printf '\\n--- plan.md (survived pause/resume) ---\\n'",
        f"cat {quoted_ws}/plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify /workspace and Claude Code state survived")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def show_final_workspace(sandbox: Sandbox, workspace: str) -> None:
    quoted = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted}",
        f"test ! -f {quoted}/progress.md || "
        f"(printf '\\n--- progress.md ---\\n' && cat {quoted}/progress.md)",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "inspect final workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["CC_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_api_key()

    cc_env = build_cc_env()
    turn_1 = TURN_1_PROMPT.format(workspace=args.workspace)
    turn_2 = TURN_2_PROMPT.format(workspace=args.workspace)

    print(f"Creating sandbox from template: {template_id}")
    # SECURITY: like run_claude_code.py this demo keeps egress open. The
    # pause() snapshot also captures the in-VM env. For shared clusters prefer
    # the default-deny + vault pattern in network_policy.py.
    sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)

    try:
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "claude --version", timeout=60)
        ensure_success(version_result, "check Claude Code version")
        print(f"Claude Code version: {getattr(version_result, 'stdout', '').strip()}")

        print("\n=== Turn 1: create plan.md ===\n")
        result_1 = run_turn(
            sandbox, args.workspace, turn_1, args.model,
            args.effort, args.exec_timeout, cc_env,
        )
        ensure_success(result_1, "run Claude Code turn 1")

        print(f"\nPausing sandbox {sandbox_id} (snapshotting VM + rootfs)...")
        paused_id = sandbox.pause()
        if isinstance(paused_id, str) and paused_id:
            sandbox_id = paused_id
            print(f"Paused. Resume handle: {sandbox_id}")
            print(f"\nReconnecting to {sandbox_id}...")
            sandbox = Sandbox.connect(sandbox_id=sandbox_id)
            print("Reconnected after resume.")
        else:
            # Local CubeSandbox (and some self-hosted deployments) return a
            # boolean from pause() instead of a reconnect handle.  The sandbox
            # stays alive — skip the reconnect step and continue in-process.
            print(
                "Pause returned a boolean (local / self-hosted CubeSandbox) — "
                "skipping reconnect, sandbox is still alive."
            )

        print("\n=== Verifying persistence after resume ===\n")
        assert_state_survived(sandbox, args.workspace, args.cc_state_dir)

        print("\n=== Turn 2: continue the work ===\n")
        result_2 = run_turn(
            sandbox, args.workspace, turn_2, args.model,
            args.effort, args.exec_timeout, cc_env,
        )
        ensure_success(result_2, "run Claude Code turn 2")

        show_final_workspace(sandbox, args.workspace)

        exit_code = getattr(result_2, "exit_code", 0)
        return 0 if exit_code is None else int(exit_code)
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
                print(f"\nSandbox {sandbox_id} killed.")
            except Exception as exc:
                print(f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                      file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
