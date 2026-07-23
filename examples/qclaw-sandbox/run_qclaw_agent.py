#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

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

DEFAULT_PROMPT_TEMPLATE = (
    "Inspect the project in {workspace}, run python3 app.py, and write a "
    "concise summary of the result to {workspace}/result.md."
)


def default_prompt(workspace: str) -> str:
    return DEFAULT_PROMPT_TEMPLATE.format(workspace=workspace)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="在 CubeSandbox 中运行一次性 OpenClaw 任务。"
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox 模板 ID。",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="传递给 OpenClaw 的提示词，默认为工作区冒烟测试任务。",
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
        "--no-seed",
        action="store_true",
        help="跳过在沙箱工作区写入演示文件。",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="输出原始响应流而非格式化后的文本。",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = default_prompt(args.workspace)
    if args.raw:
        os.environ["QCLAW_STREAM_RAW"] = "1"
    return args


def seed_project(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/README.md <<'EOF'
# CubeSandbox OpenClaw Smoke Project

This tiny project exists so OpenClaw has a deterministic task to run.
EOF
cat > {quoted_workspace}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + OpenClaw")


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
    provider = qclaw_provider()
    require_provider_key(provider)

    qclaw_env = build_qclaw_env()

    print(f"Creating sandbox from template: {template_id}")
    result = None
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        # 验证 OpenClaw CLI 已安装
        version_result = run_command(sandbox, "openclaw --version", timeout=60)
        ensure_success(version_result, "check OpenClaw version")
        print(f"OpenClaw version: {getattr(version_result, 'stdout', '').strip()}")

        # 启动 OpenClaw 网关
        print("\nStarting OpenClaw gateway...")
        start_gateway(sandbox)

        print("Waiting for gateway to be ready...")
        if not wait_gateway_ready(sandbox, max_wait=30):
            print("Gateway failed to become ready. Status:")
            print(gateway_status(sandbox))
            return 1
        print("Gateway is ready.")

        token = get_gateway_token(sandbox)
        if not token:
            print("ERROR: Could not read gateway token.")
            return 1

        if not args.no_seed:
            seed_project(sandbox, args.workspace, timeout=60)
            print(f"Seeded demo project in {args.workspace}")

        print(f"\nRunning OpenClaw task (provider: {provider})...\n")
        result = send_prompt_via_gateway(
            sandbox,
            args.prompt,
            token,
            timeout=args.exec_timeout,
        )

        show_workspace_result(sandbox, args.workspace, timeout=60)

    exit_code = getattr(result, "exit_code", 1)
    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
