"""
在 CubeSandbox 沙箱中无头运行 Claude Code。

创建沙箱、配置 Claude Code 环境、执行提示任务并输出结果。
支持开放出口（直连）和 vault（CubeEgress 密钥注入）两种模式。
"""

import argparse  # 命令行参数解析
import os
import sys

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox  # CubeSandbox 沙箱 SDK

from _common import run_command, setup_sandbox, CC_USER
from env_utils import build_claude_env, claude_command, get_provider

load_dotenv()  # 加载 .env 文件中的环境变量


def main():
    # ── 解析命令行参数 ────────────────────────────────────────────
    parser = argparse.ArgumentParser(
        description="在 CubeSandbox 沙箱中运行 Claude Code"
    )
    parser.add_argument(
        "prompt", nargs="?", default="Say hello and report the current date.",
        help="交给 Claude Code 执行的任务提示"
    )
    parser.add_argument(
        "--workdir", default=f"/home/{CC_USER}/workspace",
        help="沙箱内的工作目录"
    )
    parser.add_argument(
        "--template-id", default=os.getenv("CUBE_TEMPLATE_ID"),
        help="CubeSandbox 模板 ID"
    )
    parser.add_argument(
        "--timeout", type=int, default=600,
        help="沙箱执行超时时间（秒）"
    )
    parser.add_argument(
        "--no-approve", action="store_true",
        help="不要自动批准文件编辑和命令执行（注意：在 --print 无头模式下，"
             "需要审批的操作将被跳过而非交互式提示）"
    )
    args = parser.parse_args()

    # 检查模板 ID 是否已设置
    template_id = args.template_id or os.getenv("CUBE_TEMPLATE_ID")
    if not template_id:
        print("错误：未设置 CUBE_TEMPLATE_ID。请通过 --template-id 参数或环境变量设置。")
        sys.exit(1)

    # 获取 LLM 提供商信息并构建环境变量
    provider = get_provider()
    claude_env = build_claude_env()

    print(f"提供商: {provider}")
    print(f"模板: {template_id}")
    print(f"提示: {args.prompt[:80]}{'...' if len(args.prompt) > 80 else ''}")

    # ── 创建沙箱 ───────────────────────────────────────────────────
    print("\n正在创建沙箱 ...")
    sandbox = None
    sandbox_id = None
    sandbox = Sandbox.create(
        template_id,
        timeout=args.timeout,
    )
    sandbox_id = sandbox.sandbox_id
    print(f"沙箱已创建: {sandbox_id}")

    try:
        # ── 初始化沙箱（创建用户、安装 Claude Code、注入环境变量）──
        print(f"正在创建用户 '{CC_USER}' ...")
        try:
            setup_sandbox(sandbox, claude_env, args.workdir)
        except RuntimeError as e:
            print(f"设置失败: {e}")
            sys.exit(1)

        # ── 以非 root 用户身份运行 Claude Code ────────────────────
        cmd = claude_command(
            args.prompt, args.workdir,
            approve=not args.no_approve,
            user=CC_USER,
            env_vars=claude_env,
        )
        print(f"\n正在运行: claude --print '...' (以 {CC_USER} 身份)")
        result = run_command(sandbox, cmd, user=CC_USER, timeout=max(30, args.timeout - 60))

        if result.exit_code == 0:
            print("\n" + "=" * 60)
            print(result.stdout)  # 标准输出
            if result.stderr:
                print("\n[stderr]")
                print(result.stderr)
        else:
            print(f"\nClaude Code 退出，返回码 {result.exit_code}")
            print("STDOUT:", result.stdout)
            print("STDERR:", result.stderr)
            sys.exit(1)

    finally:
        # ── 清理：销毁沙箱 ─────────────────────────────────────────
        if sandbox is not None:
            print(f"\n正在销毁沙箱 {sandbox_id} ...")
            sandbox.kill()
            print("完成。")


if __name__ == "__main__":
    main()
