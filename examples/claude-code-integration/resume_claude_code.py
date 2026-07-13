"""
使用 CubeSandbox 快照暂停/恢复 Claude Code 会话。

演示有状态复用：启动 Claude Code 会话 → 暂停沙箱 → 稍后恢复，
对话上下文和文件更改都会被保留。
"""

import argparse
import os
import sys

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

from _common import run_command, setup_sandbox, CC_USER
from env_utils import build_claude_env, claude_command

load_dotenv()


def main():
    parser = argparse.ArgumentParser(
        description="在 CubeSandbox 中暂停/恢复 Claude Code"
    )
    parser.add_argument(
        "prompt",
        nargs="?",
        default="Create a file hello.txt with 'Hello from Claude Code!' inside.",
        help="交给 Claude Code 执行的任务提示",
    )
    parser.add_argument("--resume-from", default=None, help="从已有的沙箱 ID 恢复会话")
    parser.add_argument(
        "--template-id",
        default=os.getenv("CUBE_TEMPLATE_ID"),
        help="CubeSandbox 模板 ID",
    )
    parser.add_argument(
        "--timeout", type=int, default=600, help="沙箱执行超时时间（秒）"
    )
    parser.add_argument(
        "--no-cleanup", action="store_true", help="保持沙箱存活而不暂停它"
    )
    args = parser.parse_args()

    template_id = args.template_id or os.getenv("CUBE_TEMPLATE_ID")
    if not template_id:
        print("错误：未设置 CUBE_TEMPLATE_ID。")
        sys.exit(1)

    claude_env = build_claude_env()
    workdir = f"/home/{CC_USER}/workspace"

    # ── 创建新沙箱或恢复已有沙箱 ──────────────────────────────────
    sandbox = None
    sandbox_id = None
    if args.resume_from:
        sandbox_id = args.resume_from
        print(f"正在恢复沙箱: {sandbox_id}")
        sandbox = Sandbox.connect(sandbox_id)
    else:
        print("正在创建新沙箱 ...")
        sandbox = Sandbox.create(template_id, timeout=args.timeout)
        sandbox_id = sandbox.sandbox_id
        print(f"沙箱已创建: {sandbox_id}")

        try:
            setup_sandbox(sandbox, claude_env, workdir)
        except Exception:
            if sandbox is not None:
                sandbox.kill()
            raise

    try:
        # ── 运行 Claude Code ───────────────────────────────────────
        cmd = claude_command(
            args.prompt, workdir, env_vars=claude_env, approve=True, user=CC_USER
        )
        print(f"\n正在运行 Claude Code（以 {CC_USER} 身份）...")
        result = run_command(
            sandbox, cmd, user=CC_USER, timeout=max(30, args.timeout - 60)
        )

        if result.exit_code == 0:
            print("\n" + "=" * 60)
            print(result.stdout)
        else:
            raise RuntimeError(
                f"Claude Code 退出码 {result.exit_code}: {result.stderr}"
            )

        # ── 暂停沙箱 ──────────────────────────────────────────────
        if not args.no_cleanup:
            print(f"\n正在暂停沙箱 {sandbox_id} ...")
            sandbox.pause()
            print("沙箱已暂停。以后可使用以下命令恢复:")
            print(f"  python resume_claude_code.py --resume-from {sandbox_id}")
        else:
            print(f"\n沙箱保持运行: {sandbox_id}")

    except KeyboardInterrupt:
        print("已中断。")
        if not args.no_cleanup and sandbox is not None:
            sandbox.kill()
        raise
    except Exception as e:
        print(f"错误: {e}")
        if not args.no_cleanup and sandbox is not None:
            sandbox.kill()
        sys.exit(1)


if __name__ == "__main__":
    main()
