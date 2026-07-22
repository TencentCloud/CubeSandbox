# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for sandbox_exec.py.

These tests are fully offline: no CubeSandbox cluster or LLM credentials are
needed. The Sandbox SDK is mocked via unittest.mock so test order cannot leak
state.
"""

from __future__ import annotations

import os
import stat
import sys
import tempfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

# Import the module under test; apply env scrubbing at import time so no
# ambient CUBE_* / E2B_* state leaks between test runs.
def _clear_env():
    for key in list(os.environ):
        if key.startswith("CUBE_") or key.startswith("E2B_"):
            os.environ.pop(key, None)


_clear_env()

import sandbox_exec  # noqa: E402  (import after env scrub)

# Import Sandbox directly for mocking at class level
from e2b_code_interpreter import Sandbox


class TestExecApi:
    def test_exec_code_returns_stdout_on_success(self):
        mock_result = MagicMock()
        mock_result.exit_code = 0
        mock_result.stdout = "42\n"
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            output = sandbox_exec.exec_code("print(1+1)")
        assert output == "42\n"

    def test_exec_code_returns_stderr_on_failure(self):
        mock_result = MagicMock()
        mock_result.exit_code = 1
        mock_result.stderr = "SyntaxError\n"
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            output = sandbox_exec.exec_code("raise Exception()")
        assert "[error]" in output
        assert "exit code 1" in output  # Sanitized error message

    def test_exec_code_installs_pip_first(self):
        results = []

        def run_side_effect(cmd, timeout=None):
            results.append(cmd)
            mock_result = MagicMock()
            mock_result.exit_code = 0
            mock_result.stdout = "ok\n"
            return mock_result

        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.side_effect = run_side_effect

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            sandbox_exec.exec_code("print(1)", pip_packages=["requests"])

        assert len(results) == 2
        assert "pip install" in results[0]
        assert "requests" in results[0]

    def test_exec_code_reports_pip_error(self):
        mock_result = MagicMock()
        mock_result.exit_code = 1
        mock_result.stderr = "pip error details"

        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            output = sandbox_exec.exec_code("print(1)", pip_packages=["nonexistent-pkg-xyz"])

        assert "[pip error]" in output
        assert "exit 1" in output  # Sanitized

    def test_exec_file_works_with_allowed_path(self):
        mock_result = MagicMock()
        mock_result.exit_code = 0
        mock_result.stdout = "hello\n"

        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        # Create a temp file and add its parent to allowed dirs
        with tempfile.TemporaryDirectory() as tmpdir:
            test_file = Path(tmpdir) / "test.py"
            test_file.write_text("print('hello')")

            # Override allowed dirs for this test
            original_dirs = sandbox_exec._ALLOWED_READ_DIRS.copy()
            sandbox_exec._ALLOWED_READ_DIRS.append(Path(tmpdir))

            try:
                with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
                    output = sandbox_exec.exec_file(str(test_file))
                assert output == "hello\n"
                mock_sandbox.files.write.assert_called_once()
            finally:
                sandbox_exec._ALLOWED_READ_DIRS[:] = original_dirs

    def test_exec_file_rejects_path_outside_allowed_dirs(self):
        with patch.object(sandbox_exec, "_get_sandbox"):
            # /etc is definitely not in allowed dirs
            output = sandbox_exec.exec_file("/etc/passwd")
        assert "[error]" in output
        assert "not in allowed directories" in output

    def test_exec_file_rejects_symlink_outside_dirs(self):
        # The symlink check happens after path prefix check, so a symlink
        # outside allowed dirs gets rejected with "not in allowed directories"
        with patch.object(sandbox_exec, "_get_sandbox"):
            output = sandbox_exec.exec_file("/etc/hostname")
        assert "[error]" in output

    def test_exec_file_handles_symlink_inside_allowed_dirs(self):
        # Test the symlink rejection by directly checking the validation logic
        with tempfile.TemporaryDirectory() as tmpdir:
            target = Path(tmpdir) / "target.py"
            target.write_text("print('hello')")
            link = Path(tmpdir) / "link.py"
            link.symlink_to(target)

            # The symlink is inside allowed dirs, but should still be rejected
            # because symlinks are not allowed regardless of directory
            original_dirs = sandbox_exec._ALLOWED_READ_DIRS.copy()
            sandbox_exec._ALLOWED_READ_DIRS.append(Path(tmpdir))

            try:
                # Mock _get_sandbox to avoid actual sandbox creation
                mock_sb = MagicMock()
                mock_result = MagicMock()
                mock_result.exit_code = 0
                mock_result.stdout = "ok"
                mock_sb.commands.run.return_value = mock_result

                with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sb):
                    output = sandbox_exec.exec_file(str(link))
            finally:
                sandbox_exec._ALLOWED_READ_DIRS[:] = original_dirs

        assert "[error]" in output
        assert "symlink" in output

    def test_exec_file_handles_unicode_decode_error(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = Path(tmpdir) / "binary.bin"
            path.write_bytes(b"\x80\x81\x82")  # Invalid UTF-8

            # Add to allowed dirs
            original_dirs = sandbox_exec._ALLOWED_READ_DIRS.copy()
            sandbox_exec._ALLOWED_READ_DIRS.append(Path(tmpdir))

            try:
                with patch.object(sandbox_exec, "_get_sandbox"):
                    output = sandbox_exec.exec_file(str(path))
            finally:
                sandbox_exec._ALLOWED_READ_DIRS[:] = original_dirs

        assert "[error]" in output
        assert "UTF-8" in output

    def test_exec_cmd_returns_stdout_on_success(self):
        mock_result = MagicMock()
        mock_result.exit_code = 0
        mock_result.stdout = "total 0\n"

        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            output = sandbox_exec.exec_cmd("ls -la /workspace")

        assert output == "total 0\n"

    def test_exec_cmd_returns_stderr_on_failure(self):
        mock_result = MagicMock()
        mock_result.exit_code = 1
        mock_result.stderr = "not found\n"

        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = mock_result

        with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
            output = sandbox_exec.exec_cmd("ls /nonexistent")

        assert "[error]" in output
        assert "exit code 1" in output  # Sanitized

    def test_exec_cmd_rejects_empty_command(self):
        with patch.object(sandbox_exec, "_get_sandbox"):
            output = sandbox_exec.exec_cmd("")
        assert "[error]" in output
        assert "empty" in output

    def test_exec_cmd_rejects_oversized_command(self):
        long_cmd = "x" * (sandbox_exec.MAX_COMMAND_LENGTH + 1)
        with patch.object(sandbox_exec, "_get_sandbox"):
            output = sandbox_exec.exec_cmd(long_cmd)
        assert "[error]" in output
        assert "exceeds" in output

    def test_exec_cmd_rejects_non_string(self):
        output = sandbox_exec.exec_cmd(123)  # type: ignore
        assert "[error]" in output


class TestGetSandbox:
    def test_raises_when_template_id_unset(self):
        sandbox_exec.TEMPLATE_ID = ""

        with pytest.raises(SystemExit):
            sandbox_exec._get_sandbox()

    def test_reconnects_from_session_file(self):
        mock_sandbox = MagicMock()
        mock_sandbox.sandbox_id = "reconn-456"
        mock_sandbox.set_timeout = MagicMock()

        with patch.object(sandbox_exec, "_read_session", return_value="reconn-456"):
            with patch.object(
                Sandbox, "connect", return_value=mock_sandbox
            ):
                sandbox_exec._sandbox = None
                sb = sandbox_exec._get_sandbox()

        assert sb.sandbox_id == "reconn-456"

    def test_reuses_in_process_cache(self):
        cached = MagicMock()
        cached.set_timeout = MagicMock()
        sandbox_exec._sandbox = cached

        sb = sandbox_exec._get_sandbox()
        assert sb is cached
        cached.set_timeout.assert_called_once()

    def test_session_helpers_reject_symlink(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            symlink = Path(tmpdir) / "symlink"
            target = Path(tmpdir) / "target"
            target.write_text("sbid")
            symlink.symlink_to(target)

            original_session_file = sandbox_exec.SESSION_FILE
            sandbox_exec.SESSION_FILE = symlink
            try:
                result = sandbox_exec._read_session()
                assert result is None
            finally:
                sandbox_exec.SESSION_FILE = original_session_file

    def test_write_session_creates_valid_session_file(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            session_file = Path(tmpdir) / "session"

            sandbox_exec.SESSION_FILE = session_file
            sandbox_exec._write_session("test-sandbox-id")

            # Verify the file contains the correct sandbox ID
            assert session_file.read_text() == "test-sandbox-id"
            # Verify permissions are 0600
            assert session_file.stat().st_mode & 0o777 == 0o600


class TestRStderr:
    def test_returns_stderr_when_present(self):
        mock_result = MagicMock()
        mock_result.stderr = "error message"

        result = sandbox_exec.r_stderr(mock_result)
        assert result == "error message"

    def test_returns_exit_code_when_no_stderr(self):
        mock_result = MagicMock()
        mock_result.stderr = None
        mock_result.exit_code = 42

        result = sandbox_exec.r_stderr(mock_result)
        assert result == "exit code 42"

    def test_returns_unknown_error_when_no_attrs(self):
        mock_result = MagicMock(spec=[])

        result = sandbox_exec.r_stderr(mock_result)
        assert result == "unknown error"

    def test_truncates_long_stderr(self):
        mock_result = MagicMock()
        mock_result.stderr = "x" * 3000

        result = sandbox_exec.r_stderr(mock_result)
        assert len(result) <= 2048


class TestCleanup:
    def test_kills_sandbox_and_clears_session(self):
        mock_sb = MagicMock()
        mock_sb.kill = MagicMock()
        sandbox_exec._sandbox = mock_sb

        with tempfile.TemporaryDirectory() as tmpdir:
            session_file = Path(tmpdir) / f"cubesandbox_codebuddy_session_{os.getuid()}"
            session_file.write_text("sb-to-kill")
            original_session_file = sandbox_exec.SESSION_FILE
            sandbox_exec.SESSION_FILE = session_file
            try:
                sandbox_exec.cleanup()

                mock_sb.kill.assert_called_once()
                assert sandbox_exec._sandbox is None
                assert not session_file.exists()
            finally:
                sandbox_exec.SESSION_FILE = original_session_file

    def test_no_error_when_no_sandbox(self):
        sandbox_exec._sandbox = None
        sandbox_exec.cleanup()  # must not raise

    def test_no_error_when_kill_fails(self):
        mock_sb = MagicMock()
        mock_sb.kill.side_effect = Exception("kill failed")
        sandbox_exec._sandbox = mock_sb
        sandbox_exec.cleanup()  # must not raise
        assert sandbox_exec._sandbox is None

    def test_clears_session_even_without_sandbox(self):
        sandbox_exec._sandbox = None

        with tempfile.TemporaryDirectory() as tmpdir:
            session_file = Path(tmpdir) / f"cubesandbox_codebuddy_session_{os.getuid()}"
            session_file.write_text("orphan-session")
            original_session_file = sandbox_exec.SESSION_FILE
            sandbox_exec.SESSION_FILE = session_file
            try:
                sandbox_exec.cleanup()
                assert not session_file.exists()
            finally:
                sandbox_exec.SESSION_FILE = original_session_file

    def test_cleanup_is_idempotent(self):
        sandbox_exec._sandbox = None
        sandbox_exec.cleanup()  # must not raise
        sandbox_exec.cleanup()  # must not raise


class TestPathValidation:
    def test_allows_explicitly_configured_dirs(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            test_file = Path(tmpdir) / "test.py"
            test_file.write_text("print('hello')")

            mock_sandbox = MagicMock()
            mock_result = MagicMock()
            mock_result.exit_code = 0
            mock_result.stdout = "hello\n"
            mock_sandbox.commands.run.return_value = mock_result

            original_dirs = sandbox_exec._ALLOWED_READ_DIRS.copy()
            sandbox_exec._ALLOWED_READ_DIRS.append(Path(tmpdir))

            try:
                with patch.object(sandbox_exec, "_get_sandbox", return_value=mock_sandbox):
                    output = sandbox_exec.exec_file(str(test_file))
                assert output == "hello\n"
            finally:
                sandbox_exec._ALLOWED_READ_DIRS[:] = original_dirs

    def test_rejects_absolute_path_not_in_allowed_dirs(self):
        with patch.object(sandbox_exec, "_get_sandbox"):
            output = sandbox_exec.exec_file("/var/log/messages")
        assert "[error]" in output
