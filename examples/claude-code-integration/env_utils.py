"""
CubeSandbox 上 Claude Code 的环境变量与凭据工具函数。

配置 LLM 提供商设置，构建在沙箱内运行 Claude Code 所需的
shell 环境变量。支持 DeepSeek、Anthropic 和 OpenAI 兼容的提供商。
"""

import os
import shlex  # shell 命令安全引用
from dotenv import load_dotenv

load_dotenv()  # 从 .env 文件加载环境变量

# ── 各 LLM 提供商的默认模型配置 ──────────────────────────────────

# 默认模型映射表
PROVIDER_DEFAULT_MODEL = {
    "deepseek": "deepseek-v4-pro",
    "anthropic": "claude-sonnet-4-6",
    "openai": "gpt-4o",
}

# 各提供商所需的环境变量配置
PROVIDER_ENV_VARS = {
    "deepseek": {
        "ANTHROPIC_AUTH_TOKEN": os.getenv("ANTHROPIC_AUTH_TOKEN", ""),
        "ANTHROPIC_BASE_URL": os.getenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic"),
        "ANTHROPIC_MODEL": os.getenv("ANTHROPIC_MODEL", "deepseek-v4-pro"),
    },
    "anthropic": {
        "ANTHROPIC_AUTH_TOKEN": os.getenv("ANTHROPIC_AUTH_TOKEN", ""),
        "ANTHROPIC_BASE_URL": os.getenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
        "ANTHROPIC_MODEL": os.getenv("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
    },
    "openai": {
        "OPENAI_API_KEY": os.getenv("OPENAI_API_KEY", ""),
        "OPENAI_BASE_URL": os.getenv("OPENAI_BASE_URL", ""),
    },
}


def get_provider():
    """返回当前激活的 LLM 提供商名称（小写）。"""
    return os.getenv("CC_PROVIDER", "deepseek").lower()


def get_model():
    """返回当前提供商使用的模型名称。"""
    provider = get_provider()
    env_model = os.getenv("CC_MODEL", "")
    if env_model:
        return env_model  # 优先使用环境变量指定的模型
    return PROVIDER_DEFAULT_MODEL.get(provider, "deepseek-v4-pro")


def build_claude_env():
    """构造在沙箱中运行 Claude Code 所需的环境变量字典。"""
    provider = get_provider()
    if provider not in PROVIDER_ENV_VARS:
        raise ValueError(
            f"不支持的提供商: {provider}。"
            f"请选择: {', '.join(PROVIDER_ENV_VARS.keys())}"
        )

    # 复制该提供商的环境变量配置
    env_vars = dict(PROVIDER_ENV_VARS[provider])
    env_vars["ANTHROPIC_MODEL"] = get_model()
    # 过滤掉空值
    return {k: v for k, v in env_vars.items() if v}


def claude_command(prompt, workdir="/workspace", approve=True, user="root", env_vars=None):
    """构建用于无头模式执行的 Claude Code CLI 命令。

    参数:
        prompt: 交给 Claude Code 执行的任务提示。
        workdir: 沙箱内的工作目录。
        approve: 若为 True，自动批准文件编辑和命令执行。
        user: 执行命令的用户。
        env_vars: 运行前需要设置的环境变量字典。
    """
    # 先 cd 到工作目录，再设置环境变量
    prefix = [f"cd {shlex.quote(workdir)}"]
    if env_vars:
        prefix.extend(f"export {k}={shlex.quote(str(v))}" for k, v in env_vars.items())

    claude_args = ["claude"]
    # 不同用户使用不同的权限标志
    if approve and user != "root":
        claude_args.append("--dangerously-skip-permissions")
    elif approve and user == "root":
        claude_args.append("--permission-mode acceptEdits")
    else:
        # approve=False：使用 default 模式。在 --print 无头模式下，
        # 需要审批的操作将被跳过而非挂起等待输入，避免无限卡死。
        claude_args.append("--permission-mode default")
    # --print 表示无头模式（非交互式），--output-format text 输出纯文本
    claude_args.extend(["--print", "--output-format", "text", shlex.quote(prompt)])

    claude_cmd = " ".join(claude_args)
    # 用 && 串联所有命令
    return " && ".join(prefix + [claude_cmd])


def env_export_string(env_vars):
    """构建 shell 安全的 export 命令字符串（每行一个变量）。

    使用 shlex.quote 对值进行转义，防止包含特殊字符的值破坏命令。
    """
    return "\n".join(
        f"export {k}={shlex.quote(str(v))}" for k, v in env_vars.items()
    )


def resolve_llm_host():
    """从环境变量中解析 LLM API 的主机名，用于出口规则配置。"""
    base = os.getenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic")
    from urllib.parse import urlparse
    return urlparse(base).hostname or "api.deepseek.com"


def provider_inject():
    """构造 CubeEgress 的凭据注入配置（Inject 对象列表）。

    当流量经过 CubeEgress 出口代理时，自动将占位符凭据替换为真实凭据，
    这样沙箱内部永远不会接触真实的 API 密钥。
    """
    from cubesandbox import Inject

    provider = get_provider()
    token = os.getenv("ANTHROPIC_AUTH_TOKEN", "")

    # Authorization 请求头的 Bearer Token 注入
    injects = [
        Inject(
            header="Authorization",
            secret=f"Bearer {token}",
            format="${SECRET}",  # 占位符格式，将被替换为真实值
        )
    ]

    # Anthropic 需要额外的 API 版本号请求头
    if provider == "anthropic":
        injects.append(
            Inject(
                header="anthropic-version",
                secret="2023-06-01",
            )
        )

    return injects
