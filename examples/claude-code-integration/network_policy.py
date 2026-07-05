"""
使用 CubeEgress 网络策略安全运行 Claude Code。

本脚本演示 CubeEgress 的凭据注入和出口规则功能。
沙箱内放置占位 API Key，真实密钥由 CubeEgress 在 TLS 传输过程中注入。

默认 allow_internet_access=False（安全模式）：沙箱无法直接上网，
仅 LLM API 流量经 CubeEgress 放行并注入密钥。
首次在线安装 Claude Code 需加 --allow-internet 标志，
或使用 Dockerfile 构建预装镜像（推荐）。
"""

import argparse  # 命令行参数解析
import os
import sys

from dotenv import load_dotenv
from cubesandbox import Sandbox, Rule, Match, Action  # CubeSandbox 原生 SDK

from env_utils import (
    build_claude_env,
    claude_command,
    get_provider,
    resolve_llm_host,
    provider_inject,
    env_export_string,
)

load_dotenv()  # 加载 .env 文件中的环境变量

CC_USER = "dev"  # 沙箱内运行 Claude Code 的非 root 用户名


def run_command(sandbox, cmd, user="root", timeout=300):
    """在沙箱中执行一条 shell 命令，返回 CommandResult。

    cubesandbox SDK 的 commands.run() 对非零退出码不抛异常，
    直接返回 CommandResult。调用者需自行检查 exit_code。
    """
    return sandbox.commands.run(cmd, user=user, timeout=timeout)


def main():
    # ── 解析命令行参数 ────────────────────────────────────────────
    parser = argparse.ArgumentParser(
        description="使用 CubeEgress 安全网络策略运行 Claude Code"
    )
    parser.add_argument(
        "prompt", nargs="?", default="What is the current date? Answer briefly.",
        help="交给 Claude Code 执行的任务提示"
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
        "--allow-internet", action="store_true",
        help="允许沙箱直接访问互联网（用于首次在线安装 Claude Code；"
             "生产环境应使用预装镜像并保持默认的 False）"
    )
    args = parser.parse_args()

    template_id = args.template_id or os.getenv("CUBE_TEMPLATE_ID")
    if not template_id:
        print("错误：未设置 CUBE_TEMPLATE_ID。")
        sys.exit(1)

    # 获取 LLM 主机地址、提供商信息和凭据注入配置
    llm_host = resolve_llm_host()
    provider = get_provider()
    injects = provider_inject()

    # ── 构建出口网络规则 ──────────────────────────────────────────
    # 默认拒绝：拦截所有出站流量
    # 仅允许通过 CubeEgress 访问 LLM API 主机，并自动注入凭据
    rules = [
        Rule(
            name="allow-llm-api",  # 规则名称（用于审计日志）
            match=Match(host=llm_host),  # 匹配需要放行的 LLM API 主机
            action=Action(allow=True, inject=injects),  # 放行并注入凭据
        )
    ]

    print(f"提供商: {provider}")
    print(f"LLM 主机（经 CubeEgress）: {llm_host}")
    print(f"凭据注入: 已启用（沙箱内只有占位符，真实密钥在线路注入）")

    # ── 创建沙箱 ────────────────────────────────────────────────────
    # 默认 allow_internet_access=False（安全模式）：沙箱无法直接上网，
    # 仅 LLM API 流量经 CubeEgress 放行并注入密钥。
    # 首次安装 Claude Code 需加 --allow-internet 标志，或使用 Dockerfile 预装镜像。
    if args.allow_internet:
        print("\n正在创建沙箱（互联网已开启，用于在线安装）...")
    else:
        print("\n正在创建沙箱（安全模式，默认拒止）...")
    sandbox = None
    sandbox_id = None
    sandbox = Sandbox.create(
        template_id,
        timeout=args.timeout,
        allow_internet_access=args.allow_internet,  # 默认 False；首次安装用 --allow-internet
        network={"rules": rules},
    )
    sandbox_id = sandbox.sandbox_id
    print(f"沙箱已创建: {sandbox_id}")

    try:
        # ── 创建非 root 用户 ─────────────────────────────────────────
        run_command(sandbox, f"id -u {CC_USER} || useradd -m -s /bin/bash {CC_USER}")
        run_command(sandbox, f"mkdir -p /home/{CC_USER}/workspace && chown -R {CC_USER}:{CC_USER} /home/{CC_USER}")

        # ── 安装 Claude Code（如果模板未预装）───────────────────────
        print("正在检查 Claude Code ...")
        result = run_command(sandbox, "which claude", timeout=10)
        if result.exit_code != 0:
            print("  未找到，正在安装 Node.js ...")
            result = run_command(sandbox,
                "curl -fsSL https://deb.nodesource.com/setup_22.x | bash - "
                "&& apt-get install -y nodejs", timeout=180)
            if result.exit_code != 0:
                print(f"Node.js 安装失败: {result.stderr}")
                print("提示：沙箱无互联网访问权限。请使用 --allow-internet 标志进行首次安装，"
                      "或使用 Dockerfile 构建预装镜像。")
                sys.exit(1)
            print("  正在安装 Claude Code CLI ...")
            result = run_command(sandbox,
                "npm install -g @anthropic-ai/claude-code", timeout=300)
            if result.exit_code != 0:
                print(f"Claude Code 安装失败: {result.stderr}")
                sys.exit(1)
        print(f"  Claude Code: {run_command(sandbox, 'claude --version', timeout=10).stdout.strip()}")

        # ── 设置占位凭据（真实密钥由 CubeEgress 在传输时注入）───
        placeholder_env = build_claude_env()
        placeholder_env["ANTHROPIC_AUTH_TOKEN"] = "sk-placeholder"
        run_command(sandbox, env_export_string(placeholder_env))

        # ── 运行 Claude Code ───────────────────────────────────────
        cmd = claude_command(args.prompt, workdir=f"/home/{CC_USER}/workspace",
                             env_vars=placeholder_env, approve=True, user=CC_USER)
        print(f"\n正在运行 Claude Code（以 {CC_USER} 身份，密钥由 CubeEgress 注入）...")
        result = run_command(sandbox, cmd, user=CC_USER, timeout=max(30, args.timeout - 60))

        if result.exit_code == 0:
            print("\n" + "=" * 60)
            print(result.stdout)
        else:
            print(f"\n退出码: {result.exit_code}")
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
