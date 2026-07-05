"""Shared helpers for Claude Code + CubeSandbox integration scripts."""

from e2b.sandbox.commands.command_handle import CommandExitException

CC_USER = "dev"


def run_command(sandbox, cmd, user="root", timeout=300):
    """Execute a shell command in the sandbox and return the result.

    Non-zero exit codes are captured rather than raised, so callers
    must check result.exit_code.
    """
    try:
        return sandbox.commands.run(cmd, user=user, timeout=timeout)
    except CommandExitException as e:
        return e


def ensure_claude(sandbox):
    """Install Node.js and Claude Code if not already present in the sandbox."""
    result = run_command(sandbox, "which claude", timeout=10)
    if result.exit_code != 0:
        print("  正在安装 Node.js ...")
        result = run_command(
            sandbox,
            "curl -fsSL https://deb.nodesource.com/setup_22.x | bash - "
            "&& apt-get install -y nodejs",
            timeout=180,
        )
        if result.exit_code != 0:
            raise RuntimeError(f"Node.js 安装失败: {result.stderr}")
        print("  正在安装 Claude Code CLI ...")
        result = run_command(
            sandbox,
            "npm install -g @anthropic-ai/claude-code",
            timeout=300,
        )
        if result.exit_code != 0:
            raise RuntimeError(f"Claude Code 安装失败: {result.stderr}")
    ver = run_command(sandbox, "node --version", timeout=10)
    print(f"  Node: {ver.stdout.strip()}")
    ver = run_command(sandbox, "claude --version", timeout=10)
    print(f"  Claude Code: {ver.stdout.strip()}")


def setup_sandbox(sandbox, claude_env, workdir):
    """Initialize a fresh sandbox: create non-root user, install Claude Code,
    inject environment variables."""
    run_command(sandbox, f"id -u {CC_USER} || useradd -m -s /bin/bash {CC_USER}")
    run_command(sandbox, f"mkdir -p {workdir}")
    run_command(sandbox, f"chown -R {CC_USER}:{CC_USER} /home/{CC_USER}")

    print("正在检查 Claude Code ...")
    ensure_claude(sandbox)

    # 环境变量由 claude_command() 在执行时内联注入（export K=V && claude ...），
    # 不在此处单独 export（每次 run_command 是新 shell，无法持久），
    # 也不写入 ~/.bashrc（避免将 API 密钥持久化到沙箱磁盘）。
