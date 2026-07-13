"""Tests for env_utils.py — environment variable and command construction."""
import shlex

import pytest

from env_utils import (
    build_claude_env,
    claude_command,
    env_export_string,
    get_model,
    get_provider,
    resolve_llm_host,
)


# ── claude_command ─────────────────────────────────────────────────────

class TestClaudeCommand:
    """Tests for claude_command() — the core command builder."""

    def test_approve_true_non_root_uses_dangerously_skip_permissions(self):
        cmd = claude_command("hello", approve=True, user="dev")
        assert "--dangerously-skip-permissions" in cmd

    def test_approve_true_root_uses_accept_edits(self):
        cmd = claude_command("hello", approve=True, user="root")
        assert "--permission-mode acceptEdits" in cmd

    def test_approve_false_uses_default_mode(self):
        """Fix: approve=False now adds --permission-mode default instead of
        no flag (which would hang in headless --print mode waiting for input)."""
        cmd = claude_command("hello", approve=False, user="dev")
        assert "--permission-mode default" in cmd

    def test_approve_false_root_uses_default_mode(self):
        cmd = claude_command("hello", approve=False, user="root")
        assert "--permission-mode default" in cmd

    def test_command_includes_print_and_output_format(self):
        cmd = claude_command("hello", approve=True, user="dev")
        assert "--print" in cmd
        assert "--output-format" in cmd
        assert "text" in cmd

    def test_command_includes_cd_to_workdir(self):
        cmd = claude_command("hello", workdir="/home/dev/workspace", approve=True, user="dev")
        assert "cd /home/dev/workspace" in cmd

    def test_command_includes_env_vars_inline(self):
        env_vars = {"ANTHROPIC_AUTH_TOKEN": "sk-test", "ANTHROPIC_BASE_URL": "https://api.test.com"}
        cmd = claude_command("hello", env_vars=env_vars, approve=True, user="dev")
        assert "export ANTHROPIC_AUTH_TOKEN=" in cmd
        assert "export ANTHROPIC_BASE_URL=" in cmd

    def test_prompt_is_shell_quoted(self):
        prompt = "hello'; rm -rf /"
        cmd = claude_command(prompt, approve=True, user="dev")
        assert shlex.quote(prompt) in cmd

    def test_commands_joined_with_and(self):
        cmd = claude_command("hello", env_vars={"KEY": "val"}, approve=True, user="dev")
        parts = cmd.split(" && ")
        # cd, export, claude
        assert len(parts) >= 3

    def test_workdir_is_shell_quoted(self):
        cmd = claude_command("hello", workdir="/path with spaces", approve=True, user="dev")
        assert shlex.quote("/path with spaces") in cmd

    def test_no_env_vars_omits_export(self):
        cmd = claude_command("hello", env_vars=None, approve=True, user="dev")
        assert "export" not in cmd


# ── env_export_string ──────────────────────────────────────────────────

class TestEnvExportString:
    """Tests for env_export_string() — shell-safe export construction."""

    def test_basic_export(self):
        result = env_export_string({"KEY": "value"})
        assert result == "export KEY=value"

    def test_multiple_exports(self):
        result = env_export_string({"A": "1", "B": "2"})
        lines = result.split("\n")
        assert "export A=1" in lines
        assert "export B=2" in lines
        assert len(lines) == 2

    def test_special_characters_quoted(self):
        result = env_export_string({"KEY": "value with spaces"})
        assert shlex.quote("value with spaces") in result

    def test_empty_dict(self):
        result = env_export_string({})
        assert result == ""

    def test_integer_value_converted(self):
        result = env_export_string({"PORT": 8080})
        assert "export PORT=8080" in result


# ── get_provider ───────────────────────────────────────────────────────

class TestGetProvider:
    """Tests for get_provider() — provider selection."""

    def test_default_provider_is_deepseek(self, monkeypatch):
        monkeypatch.delenv("CC_PROVIDER", raising=False)
        assert get_provider() == "deepseek"

    def test_provider_is_lowercase(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "ANTHROPIC")
        assert get_provider() == "anthropic"

    def test_unknown_provider_name_is_normalized(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "openai")
        assert get_provider() == "openai"

    def test_openai_is_rejected(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "openai")
        with pytest.raises(ValueError, match="Unsupported provider"):
            build_claude_env()


# ── get_model ──────────────────────────────────────────────────────────

class TestGetModel:
    """Tests for get_model() — model name resolution."""

    def test_default_model_for_deepseek(self, monkeypatch):
        monkeypatch.delenv("CC_MODEL", raising=False)
        monkeypatch.setenv("CC_PROVIDER", "deepseek")
        assert get_model() == "deepseek-v4-pro"

    def test_env_model_overrides_default(self, monkeypatch):
        monkeypatch.setenv("CC_MODEL", "custom-model")
        assert get_model() == "custom-model"

    def test_default_model_for_anthropic(self, monkeypatch):
        monkeypatch.delenv("CC_MODEL", raising=False)
        monkeypatch.setenv("CC_PROVIDER", "anthropic")
        assert get_model() == "claude-sonnet-4-6"


# ── build_claude_env ───────────────────────────────────────────────────

class TestBuildClaudeEnv:
    """Tests for build_claude_env() — environment dict construction."""

    def test_unsupported_provider_raises(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "unsupported")
        with pytest.raises(ValueError, match="Unsupported provider"):
            build_claude_env()

    def test_filters_empty_values(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "deepseek")
        env = build_claude_env()
        for k, v in env.items():
            assert v != ""

    def test_includes_model(self, monkeypatch):
        monkeypatch.setenv("CC_PROVIDER", "deepseek")
        env = build_claude_env()
        assert "ANTHROPIC_MODEL" in env


# ── resolve_llm_host ───────────────────────────────────────────────────

class TestResolveLlmHost:
    """Tests for resolve_llm_host() — URL host extraction."""

    def test_default_host(self, monkeypatch):
        monkeypatch.delenv("ANTHROPIC_BASE_URL", raising=False)
        assert resolve_llm_host() == "api.deepseek.com"

    def test_custom_url(self, monkeypatch):
        monkeypatch.setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1")
        assert resolve_llm_host() == "api.anthropic.com"

    def test_url_with_path(self, monkeypatch):
        monkeypatch.setenv("ANTHROPIC_BASE_URL", "https://api.deepseek.com/anthropic")
        assert resolve_llm_host() == "api.deepseek.com"
