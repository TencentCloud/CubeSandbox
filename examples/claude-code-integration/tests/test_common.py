"""Tests for _common.py — shared sandbox helpers."""
from unittest.mock import Mock

from e2b.sandbox.commands.command_handle import CommandExitException

from _common import run_command, setup_sandbox, ensure_claude


def make_result(exit_code=0, stdout="", stderr=""):
    """Create a mock command result."""
    r = Mock()
    r.exit_code = exit_code
    r.stdout = stdout
    r.stderr = stderr
    return r


# ── run_command ────────────────────────────────────────────────────────

class TestRunCommand:
    """Tests for run_command() — command execution with exception handling."""

    def test_returns_result_on_success(self):
        sandbox = Mock()
        expected = make_result(0, "ok", "")
        sandbox.commands.run.return_value = expected
        result = run_command(sandbox, "echo hello")
        assert result is expected

    def test_catches_command_exit_exception(self):
        sandbox = Mock()
        exc = CommandExitException(stderr="err", stdout="out", exit_code=1, error="error")
        sandbox.commands.run.side_effect = exc
        result = run_command(sandbox, "false")
        assert result is exc

    def test_passes_user_and_timeout(self):
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result()
        run_command(sandbox, "whoami", user="dev", timeout=42)
        sandbox.commands.run.assert_called_once_with("whoami", user="dev", timeout=42)

    def test_default_user_is_root(self):
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result()
        run_command(sandbox, "ls")
        sandbox.commands.run.assert_called_once_with("ls", user="root", timeout=300)


# ── setup_sandbox ──────────────────────────────────────────────────────

class TestSetupSandbox:
    """Tests for setup_sandbox() — sandbox initialization."""

    def test_does_not_write_to_bashrc(self):
        """Fix: API keys must not be persisted to ~/.bashrc inside the sandbox."""
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result(0, "", "")
        setup_sandbox(sandbox, {"ANTHROPIC_AUTH_TOKEN": "sk-secret"}, "/home/dev/workspace")
        for call_args in sandbox.commands.run.call_args_list:
            cmd = call_args[0][0]
            assert ".bashrc" not in cmd, f"Command writes to .bashrc: {cmd}"

    def test_creates_user_and_directory(self):
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result(0, "", "")
        setup_sandbox(sandbox, {}, "/home/dev/workspace")
        commands = [c[0][0] for c in sandbox.commands.run.call_args_list]
        assert any("useradd" in cmd or "id -u" in cmd for cmd in commands)
        assert any("mkdir" in cmd for cmd in commands)
        assert any("chown" in cmd for cmd in commands)

    def test_installs_claude_if_not_present(self):
        sandbox = Mock()
        sandbox.commands.run.side_effect = [
            make_result(0, "", ""),    # id -u / useradd
            make_result(0, "", ""),    # mkdir
            make_result(0, "", ""),    # chown
            make_result(1, "", ""),    # which claude → not found
            make_result(0, "", ""),    # node install
            make_result(0, "", ""),    # claude install
            make_result(0, "v22", ""), # node --version
            make_result(0, "1.0", ""), # claude --version
        ]
        setup_sandbox(sandbox, {}, "/home/dev/workspace")
        commands = [c[0][0] for c in sandbox.commands.run.call_args_list]
        assert any("npm install" in cmd for cmd in commands)

    def test_skips_install_if_claude_present(self):
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result(0, "", "")
        setup_sandbox(sandbox, {}, "/home/dev/workspace")
        commands = [c[0][0] for c in sandbox.commands.run.call_args_list]
        assert not any("npm install" in cmd for cmd in commands)


# ── ensure_claude ──────────────────────────────────────────────────────

class TestEnsureClaude:
    """Tests for ensure_claude() — Claude Code installation check."""

    def test_skips_install_when_present(self):
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result(0, "", "")
        ensure_claude(sandbox)
        commands = [c[0][0] for c in sandbox.commands.run.call_args_list]
        assert not any("npm install" in cmd for cmd in commands)

    def test_raises_on_node_install_failure(self):
        import pytest
        sandbox = Mock()
        sandbox.commands.run.side_effect = [
            make_result(1, "", ""),    # which claude → not found
            make_result(1, "", "err"), # node install → fails
        ]
        with pytest.raises(RuntimeError, match="Node.js"):
            ensure_claude(sandbox)

    def test_raises_on_claude_install_failure(self):
        import pytest
        sandbox = Mock()
        sandbox.commands.run.side_effect = [
            make_result(1, "", ""),    # which claude → not found
            make_result(0, "", ""),    # node install → ok
            make_result(1, "", "err"), # claude install → fails
        ]
        with pytest.raises(RuntimeError, match="Claude Code"):
            ensure_claude(sandbox)
