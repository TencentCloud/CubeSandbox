#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""演示 Claude Code 在 CubeSandbox 暂停/恢复中的文件系统持久化。

第一轮让 Claude Code 写入 ``/workspace/plan.md``，然后暂停沙箱。
第二轮重新连接到同一沙箱，验证 ``/workspace`` 在快照后存活，
并让 Claude Code 读取之前的结果继续工作。

注意：每次 ``claude --print`` 都是独立的无状态会话，Claude Code 不保留
跨调用的对话上下文。本演示通过文件系统（plan.md / progress.md）跨轮传递
信息，验证的是文件系统状态持久化，而非对话上下文持久化。
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from _claude_common import ensure_success, run_command, sandbox_identifier
from env_utils import (
    build_claude_env,
    claude_command,
    claude_workspace,
    int_env,
    load_local_dotenv,
    required,
    require_anthropic_key,
    shell_join,
)

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
        description="演示 Claude Code 在 CubeSandbox 暂停/恢复中的文件系统持久化。"
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--workspace",
        default=claude_workspace(),
        help="Working directory inside the sandbox. Defaults to CLAUDE_CODE_WORKSPACE.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("CLAUDE_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CLAUDE_EXEC_TIMEOUT", 900),
        help="Command timeout in seconds.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream raw JSON output instead of the concise transcript.",
    )
    return parser.parse_args()


def run_turn(
    sandbox: Sandbox,
    workspace: str,
    prompt: str,
    exec_timeout: int,
    envs: dict[str, str],
):
    command = shell_join(
        f"cd {shlex.quote(workspace)}",
        claude_command(prompt),
    )
    return run_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=exec_timeout,
        stream=True,
    )


def assert_state_survived(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"test -f {quoted_workspace}/plan.md",
        "printf '\\n--- plan.md (survived pause/resume) ---\\n'",
        f"cat {quoted_workspace}/plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify /workspace survived pause/resume")
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
    if args.raw:
        os.environ["CLAUDE_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_anthropic_key()

    claude_env = build_claude_env()
    turn_1_prompt = TURN_1_PROMPT.format(workspace=args.workspace)
    turn_2_prompt = TURN_2_PROMPT.format(workspace=args.workspace)

    print(f"Creating sandbox from template: {template_id}")
    sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)

    try:
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "claude --version", timeout=60)
        ensure_success(version_result, "check Claude Code version")
        print(f"Claude Code version: {getattr(version_result, 'stdout', '').strip()}")

        print("\n=== Turn 1: create plan.md ===\n")
        result_1 = run_turn(
            sandbox, args.workspace, turn_1_prompt, args.exec_timeout, claude_env
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

        print("\n=== Verifying persistence after resume ===\n")
        assert_state_survived(sandbox, args.workspace)

        print("\n=== Turn 2: continue the work ===\n")
        result_2 = run_turn(
            sandbox, args.workspace, turn_2_prompt, args.exec_timeout, claude_env
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
                print(
                    f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    sys.exit(main())
