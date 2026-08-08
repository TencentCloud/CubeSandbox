# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for mcp_server.py.

These tests are fully offline: no CubeSandbox cluster or LLM credentials are
needed. The Sandbox SDK is mocked via unittest.mock so test order cannot leak
state.
"""

from __future__ import annotations

import json
import os
import sys
from unittest.mock import MagicMock, patch

import pytest

# Clear ambient env before importing the module under test.
def _clear_env():
    for key in list(os.environ):
        if key.startswith("CUBE_") or key.startswith("E2B_"):
            os.environ.pop(key, None)


_clear_env()

import mcp_server  # noqa: E402  (import after env scrub)


class TestHandleRequest:
    def test_initialize(self):
        request = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}
        response = mcp_server.handle_request(request)
        assert response["jsonrpc"] == "2.0"
        assert response["id"] == 1
        assert response["result"]["protocolVersion"] == "2024-11-05"
        assert response["result"]["serverInfo"]["name"] == "cubesandbox-codebuddy-mcp"

    def test_tools_list(self):
        request = {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}
        response = mcp_server.handle_request(request)
        assert response["result"]["tools"]
        tool_names = [t["name"] for t in response["result"]["tools"]]
        assert "sandbox_run_code" in tool_names
        assert "sandbox_run_command" in tool_names
        assert "sandbox_write_file" in tool_names
        assert "sandbox_read_file" in tool_names
        assert "sandbox_reset" in tool_names

    def test_tools_call_unknown_tool(self):
        request = {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": "unknown_tool", "arguments": {}},
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "Unknown tool" in response["result"]["content"][0]["text"]

    def test_notifications_initialized_returns_none(self):
        request = {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
        assert mcp_server.handle_request(request) is None

    def test_unknown_method_returns_error(self):
        request = {"jsonrpc": "2.0", "id": 5, "method": "unknown.method", "params": {}}
        response = mcp_server.handle_request(request)
        assert response["error"]["code"] == -32601

    def test_tools_call_missing_params_is_error(self):
        request = {"jsonrpc": "2.0", "id": 5, "method": "tools/call"}
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "Invalid arguments" in response["result"]["content"][0]["text"]

    def test_tools_call_empty_params_is_error(self):
        request = {"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {}}
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True

    def test_run_code_pipes_through_python(self):
        mock_result = {"exit_code": 0, "stdout": "42\n", "stderr": ""}
        with patch.object(mcp_server, "run_command", return_value=mock_result):
            request = {
                "jsonrpc": "2.0",
                "id": 6,
                "method": "tools/call",
                "params": {"name": "sandbox_run_code", "arguments": {"code": "print(42)"}},
            }
            response = mcp_server.handle_request(request)
            assert response["result"]["isError"] is False
            assert "exit_code: 0" in response["result"]["content"][0]["text"]

    def test_command_failure_preserves_exit_code_and_both_streams(self):
        mock_result = {"exit_code": 127, "stdout": "", "stderr": "command not found"}
        with patch.object(mcp_server, "run_command", return_value=mock_result):
            request = {
                "jsonrpc": "2.0",
                "id": 7,
                "method": "tools/call",
                "params": {"name": "sandbox_run_command", "arguments": {"command": "ls /bad"}},
            }
            response = mcp_server.handle_request(request)
            assert response["result"]["isError"] is True
            text = response["result"]["content"][0]["text"]
            assert "exit_code: 127" in text
            assert "command not found" in text

    def test_write_file_reports_byte_count(self):
        mock_files = MagicMock()
        mock_sandbox = MagicMock()
        mock_sandbox.files = mock_files

        with patch.object(mcp_server, "_get_sandbox", return_value=mock_sandbox):
            request = {
                "jsonrpc": "2.0",
                "id": 8,
                "method": "tools/call",
                "params": {
                    "name": "sandbox_write_file",
                    "arguments": {"path": "/workspace/test.txt", "content": "hello world"},
                },
            }
            response = mcp_server.handle_request(request)
            assert response["result"]["isError"] is False
            assert "11 bytes" in response["result"]["content"][0]["text"]

    def test_write_file_rejects_invalid_path(self):
        request = {
            "jsonrpc": "2.0",
            "id": 9,
            "method": "tools/call",
            "params": {
                "name": "sandbox_write_file",
                "arguments": {"path": "/etc/passwd", "content": "malicious"},
            },
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "Invalid arguments" in response["result"]["content"][0]["text"]

    def test_write_file_rejects_oversized_content(self):
        large_content = "x" * (mcp_server.MAX_CONTENT_LENGTH + 1)
        request = {
            "jsonrpc": "2.0",
            "id": 10,
            "method": "tools/call",
            "params": {
                "name": "sandbox_write_file",
                "arguments": {"path": "/workspace/large.txt", "content": large_content},
            },
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "exceeds maximum length" in response["result"]["content"][0]["text"]

    def test_read_file_success(self):
        mock_sandbox = MagicMock()
        mock_sandbox.files.read.return_value = "file content"

        with patch.object(mcp_server, "_get_sandbox", return_value=mock_sandbox):
            request = {
                "jsonrpc": "2.0",
                "id": 11,
                "method": "tools/call",
                "params": {"name": "sandbox_read_file", "arguments": {"path": "/workspace/test.py"}},
            }
            response = mcp_server.handle_request(request)
            assert response["result"]["isError"] is False
            assert "file content" in response["result"]["content"][0]["text"]

    def test_read_file_rejects_invalid_path(self):
        request = {
            "jsonrpc": "2.0",
            "id": 12,
            "method": "tools/call",
            "params": {"name": "sandbox_read_file", "arguments": {"path": "/root/.ssh/id_rsa"}},
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "Invalid arguments" in response["result"]["content"][0]["text"]

    def test_run_code_rejects_empty_code(self):
        request = {
            "jsonrpc": "2.0",
            "id": 13,
            "method": "tools/call",
            "params": {"name": "sandbox_run_code", "arguments": {"code": "   "}},
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "empty" in response["result"]["content"][0]["text"]

    def test_run_code_rejects_oversized_code(self):
        large_code = "x" * (mcp_server.MAX_CODE_LENGTH + 1)
        request = {
            "jsonrpc": "2.0",
            "id": 14,
            "method": "tools/call",
            "params": {"name": "sandbox_run_code", "arguments": {"code": large_code}},
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True
        assert "exceeds maximum length" in response["result"]["content"][0]["text"]

    def test_run_command_rejects_empty_command(self):
        request = {
            "jsonrpc": "2.0",
            "id": 15,
            "method": "tools/call",
            "params": {"name": "sandbox_run_command", "arguments": {"command": ""}},
        }
        response = mcp_server.handle_request(request)
        assert response["result"]["isError"] is True

    def test_reset_cleans_up_sandbox(self):
        mock_sandbox = MagicMock()
        mock_sandbox.kill = MagicMock()
        mcp_server._sandbox = mock_sandbox

        request = {
            "jsonrpc": "2.0",
            "id": 16,
            "method": "tools/call",
            "params": {"name": "sandbox_reset", "arguments": {}},
        }
        response = mcp_server.handle_request(request)
        mock_sandbox.kill.assert_called_once()
        assert mcp_server._sandbox is None


class TestReadMcpMessage:
    def test_valid_message(self, monkeypatch):
        import io

        fake_stdin = io.StringIO('{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n')
        monkeypatch.setattr(sys, "stdin", fake_stdin)

        msg = mcp_server._read_mcp_message()
        assert msg["method"] == "initialize"

    def test_invalid_json_returns_none(self, monkeypatch):
        import io

        fake_stdin = io.StringIO("not json\n")
        monkeypatch.setattr(sys, "stdin", fake_stdin)

        msg = mcp_server._read_mcp_message()
        assert msg is None

    def test_blank_line_returns_none(self, monkeypatch):
        import io

        fake_stdin = io.StringIO("\n")
        monkeypatch.setattr(sys, "stdin", fake_stdin)

        msg = mcp_server._read_mcp_message()
        assert msg is None

    def test_eof_raises_eoferror(self, monkeypatch):
        import io

        fake_stdin = io.StringIO("")
        monkeypatch.setattr(sys, "stdin", fake_stdin)

        with pytest.raises(EOFError):
            mcp_server._read_mcp_message()

    def test_oversized_message_returns_none(self, monkeypatch):
        import io

        oversized = "x" * (mcp_server.MAX_MESSAGE_LENGTH + 1) + "\n"
        fake_stdin = io.StringIO(oversized)
        monkeypatch.setattr(sys, "stdin", fake_stdin)

        msg = mcp_server._read_mcp_message()
        assert msg is None


class TestValidation:
    def test_validate_path_allows_workspace(self):
        result = mcp_server._validate_path("/workspace/test.py")
        assert result is None

    def test_validate_path_allows_tmp(self):
        result = mcp_server._validate_path("/tmp/test.py")
        assert result is None

    def test_validate_path_rejects_etc(self):
        result = mcp_server._validate_path("/etc/passwd")
        assert result is not None
        assert "must be within" in result

    def test_validate_path_rejects_root(self):
        result = mcp_server._validate_path("/root/.ssh")
        assert result is not None

    def test_validate_path_rejects_empty(self):
        result = mcp_server._validate_path("")
        assert result is not None

    def test_validate_path_rejects_non_string(self):
        result = mcp_server._validate_path(123)
        assert result is not None

    def test_validate_timeout_bounded(self):
        assert mcp_server._validate_timeout(600) == 300  # Capped at MAX_TIMEOUT
        assert mcp_server._validate_timeout(100) == 100  # Within range
        assert mcp_server._validate_timeout(0) == 1  # Minimum 1
        assert mcp_server._validate_timeout(None) == 300  # Default

    def test_validate_string_length_rejects_oversized(self):
        result = mcp_server._validate_string_length("x" * 1000, 500, "test")
        assert result is not None
        assert "exceeds maximum" in result

    def test_validate_string_length_allows_valid(self):
        result = mcp_server._validate_string_length("hello", 100, "test")
        assert result is None


class TestGetSandbox:
    def test_refreshes_ttl_on_subsequent_call(self):
        cached = MagicMock()
        cached.set_timeout = MagicMock()
        mcp_server._sandbox = cached

        sb = mcp_server._get_sandbox()
        cached.set_timeout.assert_called_once()
        assert sb is cached

    def test_cleanup_kills_and_clears_cached_sandbox(self):
        mock_sb = MagicMock()
        mock_sb.kill = MagicMock()
        mcp_server._sandbox = mock_sb

        mcp_server._cleanup_sandbox()

        mock_sb.kill.assert_called_once()
        assert mcp_server._sandbox is None

    def test_cleanup_is_idempotent(self):
        mcp_server._sandbox = None
        mcp_server._cleanup_sandbox()  # Must not raise


def test_main_exits_cleanly_on_eof(monkeypatch):
    import io

    fake_stdin = io.StringIO("")
    monkeypatch.setattr(sys, "stdin", fake_stdin)
    monkeypatch.setattr(sys, "stdout", io.StringIO())
    monkeypatch.setattr(sys, "stderr", io.StringIO())

    mcp_server._sandbox = None
    mcp_server.main()  # must not raise


def test_main_uses_newline_delimited_json_and_cleans_up(monkeypatch):
    import io

    # Send one initialize request then EOF
    fake_stdin = io.StringIO('{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n')
    output = io.StringIO()
    monkeypatch.setattr(sys, "stdin", fake_stdin)
    monkeypatch.setattr(sys, "stdout", output)
    monkeypatch.setattr(sys, "stderr", io.StringIO())

    mock_sb = MagicMock()
    mcp_server._sandbox = mock_sb

    mcp_server.main()

    lines = [l for l in output.getvalue().strip().split("\n") if l]
    assert len(lines) == 1
    resp = json.loads(lines[0])
    assert resp["result"]["serverInfo"]["name"] == "cubesandbox-codebuddy-mcp"


def test_format_command_result_empty_both_streams():
    result = {"exit_code": 0, "stdout": "", "stderr": ""}
    formatted = mcp_server._format_command_result(result)
    assert "exit_code: 0" in formatted
    assert "(no output)" in formatted


def test_format_command_result_stderr_only():
    result = {"exit_code": 1, "stdout": "", "stderr": "error"}
    formatted = mcp_server._format_command_result(result)
    assert "exit_code: 1" in formatted
    assert "stderr:" in formatted
    assert "stdout:" not in formatted


def test_format_command_result_stdout_only():
    result = {"exit_code": 0, "stdout": "hello", "stderr": ""}
    formatted = mcp_server._format_command_result(result)
    assert "exit_code: 0" in formatted
    assert "stdout:" in formatted
    assert "stderr:" not in formatted
