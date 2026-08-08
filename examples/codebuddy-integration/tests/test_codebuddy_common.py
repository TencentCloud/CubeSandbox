# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for _codebuddy_common.py.

These tests are fully offline: no CubeSandbox cluster or LLM credentials are
needed. The helpers are tested directly without mocking SDK calls.
"""

from __future__ import annotations

import argparse
import sys
from unittest.mock import MagicMock

import pytest

import _codebuddy_common as cb_common  # noqa: E402


class TestPositiveInt:
    def test_parses_positive_integer(self):
        assert cb_common.positive_int("42") == 42

    def test_rejects_zero(self):
        with pytest.raises(argparse.ArgumentTypeError):
            cb_common.positive_int("0")

    def test_rejects_negative(self):
        with pytest.raises(argparse.ArgumentTypeError):
            cb_common.positive_int("-1")

    def test_rejects_non_integer(self):
        with pytest.raises(argparse.ArgumentTypeError):
            cb_common.positive_int("abc")


class TestRunCommand:
    def test_returns_result_on_success(self):
        mock_sandbox = MagicMock()
        mock_result = MagicMock()
        mock_result.exit_code = 0
        mock_result.stdout = "hello\n"
        mock_result.stderr = ""
        mock_sandbox.commands.run.return_value = mock_result

        result = cb_common.run_command(mock_sandbox, "echo hello")
        assert result.exit_code == 0
        assert result.stdout == "hello\n"

    def test_returns_result_on_non_zero_exit(self):
        mock_sandbox = MagicMock()
        mock_result = MagicMock()
        mock_result.exit_code = 1
        mock_result.stdout = ""
        mock_result.stderr = "error"
        mock_sandbox.commands.run.return_value = mock_result

        result = cb_common.run_command(mock_sandbox, "false")
        assert result.exit_code == 1

    def test_passes_timeout_and_cwd(self):
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = MagicMock(exit_code=0, stdout="", stderr="")

        cb_common.run_command(mock_sandbox, "ls", cwd="/tmp", timeout=30)
        mock_sandbox.commands.run.assert_called_once()
        call_kwargs = mock_sandbox.commands.run.call_args.kwargs
        assert call_kwargs["cwd"] == "/tmp"
        assert call_kwargs["timeout"] == 30

    def test_stream_mode_attaches_on_stdout_and_stderr(self):
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = MagicMock(exit_code=0, stdout="", stderr="")

        cb_common.run_command(mock_sandbox, "ls", stream=True)
        call_kwargs = mock_sandbox.commands.run.call_args.kwargs
        assert "on_stdout" in call_kwargs
        assert "on_stderr" in call_kwargs

    def test_default_user_is_user(self):
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.return_value = MagicMock(exit_code=0, stdout="", stderr="")

        cb_common.run_command(mock_sandbox, "ls")
        call_kwargs = mock_sandbox.commands.run.call_args.kwargs
        assert call_kwargs["user"] == "user"

    def test_non_envs_typeerror_propagates(self):
        mock_sandbox = MagicMock()
        mock_sandbox.commands.run.side_effect = TypeError("unexpected kwarg")

        with pytest.raises(TypeError, match="unexpected kwarg"):
            cb_common.run_command(mock_sandbox, "ls")

    def test_falls_back_to_env_kwarg_for_legacy_sdk(self):
        mock_sandbox = MagicMock()
        mock_result = MagicMock(exit_code=0, stdout="ok", stderr="")
        mock_sandbox.commands.run.side_effect = [
            TypeError("run() got an unexpected keyword argument 'envs'"),
            mock_result,
        ]

        result = cb_common.run_command(mock_sandbox, "ls", envs={"FOO": "bar"})
        assert result.exit_code == 0


class TestEnsureSuccess:
    def test_zero_exit_does_not_raise(self):
        mock_result = MagicMock(exit_code=0, stdout="ok", stderr="")
        cb_common.ensure_success(mock_result, "test action")  # must not raise

    def test_none_exit_does_not_raise(self):
        mock_result = MagicMock(exit_code=None, stdout="ok", stderr="")
        cb_common.ensure_success(mock_result, "test action")  # must not raise

    def test_non_zero_exit_raises(self):
        mock_result = MagicMock(exit_code=1, stdout="", stderr="error msg")
        with pytest.raises(SystemExit) as exc_info:
            cb_common.ensure_success(mock_result, "do something")
        assert "Failed to do something" in str(exc_info.value)
        assert "error msg" in str(exc_info.value)


class TestSandboxIdentifier:
    def test_prefers_sandbox_id(self):
        mock_sb = MagicMock()
        mock_sb.sandbox_id = "sb-123"
        mock_sb.id = "id-456"
        assert cb_common.sandbox_identifier(mock_sb) == "sb-123"

    def test_falls_back_to_id(self):
        mock_sb = MagicMock(spec=["id"])
        mock_sb.id = "id-789"
        assert cb_common.sandbox_identifier(mock_sb) == "id-789"

    def test_returns_unknown_when_neither_attr(self):
        # Use spec=[] to prevent MagicMock from auto-creating attributes
        mock_sb = MagicMock(spec=[])
        assert cb_common.sandbox_identifier(mock_sb) == "unknown"


class TestStreamWriter:
    def test_writes_plain_string(self, monkeypatch):
        output = []
        monkeypatch.setattr(sys.stdout, "write", lambda x: output.append(x))
        monkeypatch.setattr(sys.stdout, "flush", lambda: None)

        writer = cb_common.stream_writer(sys.stdout)
        writer("hello")
        assert "hello" in output

    def test_handles_chunk_with_line_attr(self, monkeypatch):
        output = []
        monkeypatch.setattr(sys.stdout, "write", lambda x: output.append(x))
        monkeypatch.setattr(sys.stdout, "flush", lambda: None)

        writer = cb_common.stream_writer(sys.stdout)
        chunk = MagicMock()
        chunk.line = "line content\n"
        writer(chunk)
        # The output list contains the string with trailing newline
        assert any("line content" in s for s in output)
