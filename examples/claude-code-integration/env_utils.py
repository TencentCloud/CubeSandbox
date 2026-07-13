"""Environment and credential helpers for running Claude Code in CubeSandbox.

Claude Code speaks the Anthropic API protocol. DeepSeek is supported through
its Anthropic-compatible endpoint; OpenAI-compatible endpoints are not.
"""

import os
import shlex
from dotenv import load_dotenv

load_dotenv()

# Provider defaults

PROVIDER_DEFAULT_MODEL = {
    "deepseek": "deepseek-v4-pro",
    "anthropic": "claude-sonnet-4-6",
}

PROVIDER_ENV_DEFAULTS = {
    "deepseek": {
        "ANTHROPIC_AUTH_TOKEN": "",
        "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
        "ANTHROPIC_MODEL": "deepseek-v4-pro",
    },
    "anthropic": {
        "ANTHROPIC_AUTH_TOKEN": "",
        "ANTHROPIC_BASE_URL": "https://api.anthropic.com",
        "ANTHROPIC_MODEL": "claude-sonnet-4-6",
    },
}


def get_provider():
    """Return the active LLM provider name in lowercase."""
    return os.getenv("CC_PROVIDER", "deepseek").lower()


def get_model():
    """Return the model selected for the active provider."""
    provider = get_provider()
    env_model = os.getenv("CC_MODEL", "")
    if env_model:
        return env_model
    return PROVIDER_DEFAULT_MODEL.get(provider, "deepseek-v4-pro")


def build_claude_env():
    """Build the environment used to run Claude Code in the sandbox."""
    provider = get_provider()
    if provider not in PROVIDER_ENV_DEFAULTS:
        raise ValueError(
            f"Unsupported provider: {provider}. "
            f"Choose one of: {', '.join(PROVIDER_ENV_DEFAULTS.keys())}"
        )

    env_vars = {key: os.getenv(key, default) for key, default in PROVIDER_ENV_DEFAULTS[provider].items()}
    env_vars["ANTHROPIC_MODEL"] = get_model()
    return {k: v for k, v in env_vars.items() if v}


def claude_command(prompt, workdir="/workspace", approve=True, user="root", env_vars=None):
    """Build a command that runs Claude Code in headless mode.

    Args:
        prompt: Task prompt passed to Claude Code.
        workdir: Working directory inside the sandbox.
        approve: Automatically approve edits and commands when true.
        user: User that runs the command.
        env_vars: Environment variables exported before execution.
    """
    prefix = [f"cd {shlex.quote(workdir)}"]
    if env_vars:
        prefix.extend(f"export {k}={shlex.quote(str(v))}" for k, v in env_vars.items())

    claude_args = ["claude"]
    if approve and user != "root":
        claude_args.append("--dangerously-skip-permissions")
    elif approve and user == "root":
        claude_args.append("--permission-mode acceptEdits")
    else:
        # In headless mode, default permissions skip actions that need approval.
        claude_args.append("--permission-mode default")
    claude_args.extend(["--print", "--output-format", "text", shlex.quote(prompt)])

    claude_cmd = " ".join(claude_args)
    return " && ".join(prefix + [claude_cmd])


def env_export_string(env_vars):
    """Build shell-safe export statements, one variable per line."""
    return "\n".join(
        f"export {k}={shlex.quote(str(v))}" for k, v in env_vars.items()
    )


def resolve_llm_host():
    """Resolve the active Anthropic-protocol API host for egress rules."""
    base = os.getenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic")
    from urllib.parse import urlparse
    return urlparse(base).hostname or "api.deepseek.com"


def provider_inject():
    """Build CubeEgress injections without exposing the real token in the VM."""
    from cubesandbox import Inject

    provider = get_provider()
    token = os.getenv("ANTHROPIC_AUTH_TOKEN", "")

    injects = [
        Inject(
            header="Authorization",
            secret=f"Bearer {token}",
            format="${SECRET}",
        )
    ]

    if provider == "anthropic":
        injects.append(
            Inject(
                header="anthropic-version",
                secret="2023-06-01",
            )
        )

    return injects
