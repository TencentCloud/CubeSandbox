# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for sandbox_exec.py — session-based cross-process sandbox reuse.

Mirrors the structure of the Claude Code integration's test_sandbox_exec.py
so the two examples can be reviewed side-by-side.
"""

from __future__ import annotations

import os
from unittest.mock import Mock, patch

import pytest

import sandbox_exec


@pytest.fixture
def temp_session(monkeypatch: pytest.MonkeyPatch, tmp_path):
    """Use a temporary session file for test isolation.

    Without this fixture, every test would write to
    ``/tmp/cubesandbox_opencode_session_<uid>`` and leave stale state for
    the next run.  CUBE_TEMPLATE_ID is patched both in the env (for real
    runs) and as a module-level variable (because the module reads it once
    at import time).
    """
    monkeypatch.setenv("CUBE_TEMPLATE_ID", "tpl-test")
    monkeypatch.setattr(sandbox_exec, "TEMPLATE_ID", "tpl-test")
    session_file = tmp_path / "session"
    monkeypatch.setattr(sandbox_exec, "SESSION_FILE", session_file)
    monkeypatch.setattr(sandbox_exec, "_sandbox", None)
    return session_file


# ── _get_sandbox ───────────────────────────────────────────────────────


class TestGetSandbox:
    """Tests for sandbox_exec._get_sandbox() — cross-process session reuse."""

    def test_creates_new_sandbox_when_no_session(self, temp_session) -> None:
        mock_instance = Mock()
        mock_instance.sandbox_id = "sb-new"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.create.return_value = mock_instance
            result = sandbox_exec._get_sandbox()
        assert result is mock_instance
        mock_cls.create.assert_called_once()
        assert temp_session.exists()
        assert temp_session.read_text() == "sb-new"

    def test_reconnects_from_session_file(self, temp_session) -> None:
        # Reuses a sandbox from a previous --keep-alive run by reading the
        # session file and calling Sandbox.connect() instead of create().
        temp_session.write_text("sb-existing")
        mock_instance = Mock()
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.connect.return_value = mock_instance
            result = sandbox_exec._get_sandbox(timeout=300)
        assert result is mock_instance
        mock_cls.connect.assert_called_once_with("sb-existing")
        mock_cls.create.assert_not_called()
        mock_instance.set_timeout.assert_called_once_with(300)

    def test_clears_stale_session_on_connect_failure(self, temp_session) -> None:
        # When reconnect fails (sandbox expired), the stale session file
        # is cleaned up and a fresh sandbox is created.
        temp_session.write_text("sb-stale")
        new_instance = Mock()
        new_instance.sandbox_id = "sb-fresh"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.connect.side_effect = Exception("not found")
            mock_cls.create.return_value = new_instance
            result = sandbox_exec._get_sandbox()
        assert result is new_instance
        assert temp_session.read_text() == "sb-fresh"

    def test_reuses_in_process_cache(self, temp_session) -> None:
        mock_instance = Mock()
        sandbox_exec._sandbox = mock_instance
        result = sandbox_exec._get_sandbox()
        assert result is mock_instance

    def test_writes_session_file_on_create(self, temp_session) -> None:
        mock_instance = Mock()
        mock_instance.sandbox_id = "sb-123"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.create.return_value = mock_instance
            sandbox_exec._get_sandbox()
        assert temp_session.read_text() == "sb-123"

    def test_raises_when_template_id_unset(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Test isolation: do NOT use temp_session which already sets TEMPLATE_ID.
        # Only patch SESSION_FILE and _sandbox so _get_sandbox() reaches the
        # TEMPLATE_ID check before the SDK is invoked.
        import pathlib
        fake_session = pathlib.Path("/dev/null")
        monkeypatch.setattr(sandbox_exec, "SESSION_FILE", fake_session)
        monkeypatch.setattr(sandbox_exec, "_sandbox", None)
        monkeypatch.delenv("CUBE_TEMPLATE_ID", raising=False)
        monkeypatch.setattr(sandbox_exec, "TEMPLATE_ID", "")
        with pytest.raises(SystemExit, match="CUBE_TEMPLATE_ID"):
            sandbox_exec._get_sandbox()

    @pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="O_NOFOLLOW unavailable")
    def test_session_helpers_reject_symlink(self, temp_session, tmp_path) -> None:
        # A symlink attack would let an attacker point us at an arbitrary
        # file before the write happens. O_NOFOLLOW + S_ISREG close that.
        target = tmp_path / "attacker-session"
        target.write_text("sb-attacker")
        temp_session.symlink_to(target)

        assert sandbox_exec._read_session() is None
        with pytest.raises(OSError):
            sandbox_exec._write_session("sb-safe")
        # The attacker-controlled file must not have been overwritten.
        assert target.read_text() == "sb-attacker"


# ── cleanup ────────────────────────────────────────────────────────────


class TestCleanup:
    """Tests for sandbox_exec.cleanup() — sandbox destruction."""

    def test_kills_sandbox_and_clears_session(self, temp_session) -> None:
        mock_instance = Mock()
        sandbox_exec._sandbox = mock_instance
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        mock_instance.kill.assert_called_once()
        assert sandbox_exec._sandbox is None
        assert not temp_session.exists()

    def test_no_error_when_no_sandbox(self, temp_session) -> None:
        sandbox_exec._sandbox = None
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        assert not temp_session.exists()

    def test_no_error_when_kill_fails(self, temp_session) -> None:
        # cleanup catches kill exceptions so a dead sandbox does not mask
        # the original error or leave the session file behind.
        mock_instance = Mock()
        mock_instance.kill.side_effect = Exception("already dead")
        sandbox_exec._sandbox = mock_instance
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        assert sandbox_exec._sandbox is None
        assert not temp_session.exists()

    def test_clears_session_even_without_sandbox(self, temp_session) -> None:
        temp_session.write_text("sb-orphan")
        sandbox_exec._sandbox = None
        sandbox_exec.cleanup()
        assert not temp_session.exists()


# ── public exec API ────────────────────────────────────────────────────


class TestExecApi:
    """Tests for the public exec_code / exec_file / exec_cmd entry points."""

    def test_exec_code_returns_stdout_on_success(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=0, stdout="hello", stderr="")
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_code("print('hello')")
        assert out == "hello"

    def test_exec_code_returns_stderr_on_failure(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=1, stdout="", stderr="boom")
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_code("1/0")
        assert "boom" in out
        assert "[error]" in out

    def test_exec_code_installs_pip_first(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.side_effect = [
            Mock(exit_code=0, stdout="", stderr=""),  # pip install
            Mock(exit_code=0, stdout="1.0", stderr=""),  # python -c
        ]
        sandbox_exec._sandbox = sb
        sandbox_exec.exec_code("import x", pip_packages=["x"])
        first_cmd = sb.commands.run.call_args_list[0][0][0]
        assert "pip install" in first_cmd

    def test_exec_code_reports_pip_error(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=1, stdout="", stderr="bad pkg")
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_code("import x", pip_packages=["x"])
        assert "[pip error]" in out
        assert "bad pkg" in out

    def test_exec_cmd_returns_stdout_on_success(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=0, stdout="root", stderr="")
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_cmd("whoami")
        assert out == "root"

    def test_exec_cmd_returns_stderr_on_failure(self, temp_session) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=2, stdout="", stderr="denied")
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_cmd("rm /etc/shadow")
        assert "[error]" in out
        assert "denied" in out

    def test_exec_file_copies_and_executes(self, temp_session, tmp_path) -> None:
        sb = Mock()
        sb.commands.run.return_value = Mock(exit_code=0, stdout="42", stderr="")
        sandbox_exec._sandbox = sb
        script = tmp_path / "x.py"
        script.write_text("print(42)")
        out = sandbox_exec.exec_file(str(script))
        assert out == "42"
        # First call writes the script, second call runs it.
        sb.files.write.assert_called_once()

    def test_exec_file_handles_missing_local_file(self, temp_session) -> None:
        sb = Mock()
        sandbox_exec._sandbox = sb
        out = sandbox_exec.exec_file("/nonexistent/path.py")
        assert "[error]" in out
        assert "cannot read" in out