#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

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

DEFAULT_PROMPT_TEMPLATE = (
    "Inspect the project in {workspace}, run python3 app.py, and write a "
    "concise summary of the result to {workspace}/result.md."
)


def default_prompt(workspace: str) -> str:
    return DEFAULT_PROMPT_TEMPLATE.format(workspace=workspace)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="在 CubeSandbox 中运行一次性 Claude Code 任务。"
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox 模板 ID，默认为 CUBE_TEMPLATE_ID。",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="传递给 Claude Code 的提示词，默认为一个工作区冒烟测试任务。",
    )
    parser.add_argument(
        "--workspace",
        default=claude_workspace(),
        help="沙箱内的工作目录，默认为 CLAUDE_CODE_WORKSPACE。",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("CLAUDE_SANDBOX_TIMEOUT", 1800),
        help="沙箱生命周期（秒），默认为 CLAUDE_SANDBOX_TIMEOUT 或 1800。",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CLAUDE_EXEC_TIMEOUT", 900),
        help="命令超时（秒），默认为 CLAUDE_EXEC_TIMEOUT 或 900。",
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="跳过在沙箱工作区写入演示文件。",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="输出原始 JSON 流而非简洁文本摘要。",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = default_prompt(args.workspace)
    if args.raw:
        os.environ["CLAUDE_STREAM_RAW"] = "1"
    return args


def seed_project(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/README.md <<'EOF'
# CubeSandbox Claude Code Smoke Project

This tiny project exists so Claude Code has a deterministic task to run.
EOF
cat > {quoted_workspace}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + Claude Code")


if __name__ == "__main__":
    main()
EOF
"""
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "seed workspace")


def show_workspace_result(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted_workspace}",
        f"test ! -f {quoted_workspace}/result.md || "
        f"(printf '\\n--- result.md ---\\n' && cat {quoted_workspace}/result.md)",
    )
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "inspect workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_anthropic_key()

    claude_env = build_claude_env()
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        claude_command(args.prompt),
    )

    print(f"Creating sandbox from template: {template_id}")
    result = None
    # SECURITY: 本演示保持默认的出站网络访问（allow_internet_access 默认为 True），
    # 并通过 envs= 将 ANTHROPIC_API_KEY 注入沙箱。被攻破的智能体可利用开放出站流量
    # 窃取该密钥。共享/生产环境请使用默认拒绝出口 + CubeEgress 凭证保险箱模式，
    # 密钥不进入虚拟机。参见 pi-agent-integration/network_policy.py。
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "claude --version", timeout=60)
        ensure_success(version_result, "check Claude Code version")
        print(f"Claude Code version: {getattr(version_result, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace, timeout=60)
            print(f"Seeded demo project in {args.workspace}")

        print("\nRunning Claude Code task...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=claude_env,
            timeout=args.exec_timeout,
            stream=True,
        )

        show_workspace_result(sandbox, args.workspace, timeout=60)

    exit_code = getattr(result, "exit_code", 1)
    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
