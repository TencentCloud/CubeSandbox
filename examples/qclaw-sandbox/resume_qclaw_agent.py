#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""演示 OpenClaw 会话在 CubeSandbox 暂停/恢复中的持久化。

第一轮让 OpenClaw 写入 ``/workspace/plan.md``，然后暂停沙箱。
第二轮重新连接到同一沙箱，验证 ``/workspace`` 和 ``/root/.openclaw/``
在快照后存活，然后继续工作。
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from _qclaw_common import (
    ensure_success,
    gateway_status,
    get_gateway_token,
    run_command,
    sandbox_identifier,
    send_prompt_via_gateway,
    start_gateway,
    wait_gateway_ready,
)
from env_utils import (
    build_qclaw_env,
    int_env,
    load_local_dotenv,
    qclaw_provider,
    qclaw_workspace,
    required,
    require_provider_key,
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
        description="演示 OpenClaw 会话在暂停/恢复中的持久化。"
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox 模板 ID。",
    )
    parser.add_argument(
        "--workspace",
        default=qclaw_workspace(),
        help="沙箱内的工作目录。",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("QCLAW_SANDBOX_TIMEOUT", 1800),
        help="沙箱生命周期（秒）。",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("QCLAW_EXEC_TIMEOUT", 900),
        help="命令超时（秒）。",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="输出原始响应流而非格式化后的文本。",
    )
    args = parser.parse_args()
    if args.raw:
        os.environ["QCLAW_STREAM_RAW"] = "1"
    return args


def run_turn(
    sandbox: Sandbox,
    prompt: str,
    token: str,
    exec_timeout: int,
):
    return send_prompt_via_gateway(
        sandbox,
        prompt,
        token,
        timeout=exec_timeout,
    )


def assert_state_survived(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"test -f {quoted_workspace}/plan.md",
        "test -d /root/.openclaw",
        "printf '\\n--- plan.md (survived pause/resume) ---\\n'",
        f"cat {quoted_workspace}/plan.md",
        "printf '\\n--- /root/.openclaw survived ---\\n'",
        "ls -la /root/.openclaw/",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify workspace and OpenClaw state survived pause/resume")
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
    provider = qclaw_provider()
    require_provider_key(provider)

    qclaw_env = build_qclaw_env()
    turn_1_prompt = TURN_1_PROMPT.format(workspace=args.workspace)
    turn_2_prompt = TURN_2_PROMPT.format(workspace=args.workspace)

    print(f"Creating sandbox from template: {template_id}")
    sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)

    try:
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "openclaw --version", timeout=60)
        ensure_success(version_result, "check OpenClaw version")
        print(f"OpenClaw version: {getattr(version_result, 'stdout', '').strip()}")

        print("\nStarting OpenClaw gateway...")
        start_gateway(sandbox)
        if not wait_gateway_ready(sandbox, max_wait=30):
            raise SystemExit("Gateway failed to become ready.")
        print("Gateway is ready.")

        token = get_gateway_token(sandbox)
        if not token:
            raise SystemExit("Could not read gateway token.")

        print("\n=== Turn 1: create plan.md ===\n")
        result_1 = run_turn(sandbox, turn_1_prompt, token, args.exec_timeout)
        ensure_success(result_1, "run OpenClaw turn 1")

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

        # 恢复后重启网关（进程已被快照但可能需要刷新）
        print("\nRestarting OpenClaw gateway after resume...")
        start_gateway(sandbox)
        if not wait_gateway_ready(sandbox, max_wait=30):
            raise SystemExit("Gateway failed to become ready after resume.")
        token = get_gateway_token(sandbox)

        print("\n=== Turn 2: continue the work ===\n")
        result_2 = run_turn(sandbox, turn_2_prompt, token, args.exec_timeout)
        ensure_success(result_2, "run OpenClaw turn 2")

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
