# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Tests for sandbox_exec.py — the standalone CLI.

Tests argument parsing, the _run wrapper, and exec_* functions
with a mock sandbox (no live deployment needed).
"""

from __future__ import annotations

import sys
from unittest.mock import MagicMock, patch

import pytest

import sandbox_exec


# ── _run wrapper ──────────────────────────────────────────────────────


class TestRunWrapper:
    def test_run_returns_result_on_success(self):
        sb = MagicMock()
        expected = MagicMock(exit_code=0, stdout="ok", stderr="")
        sb.commands.run.return_value = expected
        result = sandbox_exec._run(sb, "echo hi", timeout=10)
        assert result is expected
        sb.commands.run.assert_called_once_with("echo hi", timeout=10)

    def test_run_returns_exception_on_nonzero_exit(self):
        from e2b.sandbox.commands.command_handle import CommandExitException
        sb = MagicMock()
        exc = CommandExitException(stderr="err", stdout="", exit_code=1, error=None)
        sb.commands.run.side_effect = exc
        result = sandbox_exec._run(sb, "false", timeout=10)
        assert result is exc


# ── exec_code ─────────────────────────────────────────────────────────


class TestExecCode:
    @patch("sandbox_exec._get_sandbox")
    def test_exec_code_success(self, mock_get):
        sb = MagicMock()
        mock_result = MagicMock(exit_code=0, stdout="42\n", stderr="")
        sb.commands.run.return_value = mock_result
        mock_get.return_value = sb

        out = sandbox_exec.exec_code("print(1+1)")
        assert "42" in out

    @patch("sandbox_exec._get_sandbox")
    def test_exec_code_failure(self, mock_get):
        sb = MagicMock()
        mock_result = MagicMock(exit_code=1, stdout="", stderr="SyntaxError")
        sb.commands.run.return_value = mock_result
        mock_get.return_value = sb

        out = sandbox_exec.exec_code("invalid syntax")
        assert "[error]" in out

    @patch("sandbox_exec._get_sandbox")
    def test_exec_code_with_pip(self, mock_get):
        sb = MagicMock()
        pip_result = MagicMock(exit_code=0, stdout="", stderr="")
        code_result = MagicMock(exit_code=0, stdout="ok\n", stderr="")
        sb.commands.run.side_effect = [pip_result, code_result]
        mock_get.return_value = sb

        out = sandbox_exec.exec_code("import requests", pip_packages=["requests"])
        assert "ok" in out
        # First call: pip install, second: python3 -c
        assert sb.commands.run.call_count == 2

    @patch("sandbox_exec._get_sandbox")
    def test_exec_code_pip_failure(self, mock_get):
        sb = MagicMock()
        pip_result = MagicMock(exit_code=1, stdout="", stderr="pip error")
        sb.commands.run.return_value = pip_result
        mock_get.return_value = sb

        out = sandbox_exec.exec_code("import x", pip_packages=["nonexistent-pkg"])
        assert "[pip error]" in out


# ── exec_cmd ──────────────────────────────────────────────────────────


class TestExecCmd:
    @patch("sandbox_exec._get_sandbox")
    def test_exec_cmd_success(self, mock_get):
        sb = MagicMock()
        sb.commands.run.return_value = MagicMock(exit_code=0, stdout="file1\nfile2\n", stderr="")
        mock_get.return_value = sb

        out = sandbox_exec.exec_cmd("ls")
        assert "file1" in out

    @patch("sandbox_exec._get_sandbox")
    def test_exec_cmd_failure(self, mock_get):
        sb = MagicMock()
        sb.commands.run.return_value = MagicMock(exit_code=127, stdout="", stderr="command not found")
        mock_get.return_value = sb

        out = sandbox_exec.exec_cmd("nonexistent-cmd")
        assert "[error]" in out


# ── exec_file ─────────────────────────────────────────────────────────


class TestExecFile:
    @patch("sandbox_exec._get_sandbox")
    def test_exec_file_success(self, mock_get, tmp_path):
        script = tmp_path / "test.py"
        script.write_text("print('from file')")

        sb = MagicMock()
        sb.commands.run.return_value = MagicMock(exit_code=0, stdout="from file\n", stderr="")
        mock_get.return_value = sb

        out = sandbox_exec.exec_file(str(script))
        assert "from file" in out
        # File should be written into the sandbox
        sb.files.write.assert_called_once()
        written_path = sb.files.write.call_args[0][0]
        assert written_path == "/tmp/script.py"


# ── cleanup ───────────────────────────────────────────────────────────


class TestCleanup:
    def test_cleanup_kills_sandbox(self):
        mock_sb = MagicMock()
        sandbox_exec._sandbox = mock_sb
        sandbox_exec.cleanup()
        mock_sb.kill.assert_called_once()
        assert sandbox_exec._sandbox is None

    def test_cleanup_noop_when_none(self):
        sandbox_exec._sandbox = None
        sandbox_exec.cleanup()  # must not raise
        assert sandbox_exec._sandbox is None


# ── _get_sandbox ──────────────────────────────────────────────────────


class TestGetSandbox:
    def setup_method(self):
        sandbox_exec._sandbox = None

    def teardown_method(self):
        sandbox_exec._sandbox = None

    @patch("sandbox_exec.Sandbox")
    def test_creates_sandbox_on_first_call(self, mock_sb_cls):
        mock_sb = MagicMock()
        mock_sb_cls.create.return_value = mock_sb
        sb = sandbox_exec._get_sandbox(timeout=300)
        assert sb is mock_sb
        mock_sb_cls.create.assert_called_once()

    @patch("sandbox_exec.Sandbox")
    def test_reuses_sandbox_on_second_call(self, mock_sb_cls):
        mock_sb = MagicMock()
        mock_sb_cls.create.return_value = mock_sb
        sandbox_exec._get_sandbox(timeout=300)
        sandbox_exec._get_sandbox(timeout=300)
        assert mock_sb_cls.create.call_count == 1


# ── CLI argument parsing ──────────────────────────────────────────────


class TestArgParsing:
    @patch("sandbox_exec.exec_code")
    @patch("sandbox_exec.cleanup")
    @patch("sandbox_exec.TEMPLATE_ID", "tpl-test")
    def test_code_arg(self, mock_cleanup, mock_exec):
        mock_exec.return_value = "result"
        sys.argv = ["sandbox_exec.py", "--code", "print(1)"]
        sandbox_exec.main()
        mock_exec.assert_called_once()

    @patch("sandbox_exec.exec_cmd")
    @patch("sandbox_exec.cleanup")
    @patch("sandbox_exec.TEMPLATE_ID", "tpl-test")
    def test_cmd_arg(self, mock_cleanup, mock_exec):
        mock_exec.return_value = "result"
        sys.argv = ["sandbox_exec.py", "--cmd", "ls -la"]
        sandbox_exec.main()
        mock_exec.assert_called_once()

    @patch("sandbox_exec.exec_file")
    @patch("sandbox_exec.cleanup")
    @patch("sandbox_exec.TEMPLATE_ID", "tpl-test")
    def test_file_arg(self, mock_cleanup, mock_exec):
        mock_exec.return_value = "result"
        sys.argv = ["sandbox_exec.py", "--file", "/tmp/test.py"]
        sandbox_exec.main()
        mock_exec.assert_called_once()

    @patch("sandbox_exec.TEMPLATE_ID", "tpl-test")
    def test_keep_alive_skips_cleanup(self):
        """--keep-alive must skip cleanup() so the sandbox survives."""
        sys.argv = ["sandbox_exec.py", "--code", "print(1)", "--keep-alive"]
        with patch("sandbox_exec.exec_code", return_value="ok") as mock_exec, \
             patch("sandbox_exec.cleanup") as mock_cleanup:
            sandbox_exec.main()
            mock_exec.assert_called_once()
            mock_cleanup.assert_not_called()

    @patch("sandbox_exec.TEMPLATE_ID", "")
    def test_missing_template_id_exits(self):
        sys.argv = ["sandbox_exec.py", "--code", "print(1)"]
        with pytest.raises(SystemExit) as exc_info:
            sandbox_exec.main()
        assert exc_info.value.code == 1
