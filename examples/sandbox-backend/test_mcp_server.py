# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Tests for mcp_server.py — MCP protocol handling.

Focuses on the fixes applied per PR #765 review:
  - TTL refresh on get_sandbox() and atexit cleanup
  - EOF detection in _read_mcp_message() (prevents infinite loop)
  - Error logging instead of silent swallowing
"""

from __future__ import annotations

import io
import json
import sys
from unittest.mock import MagicMock, patch

import pytest

import mcp_server


# ── _read_mcp_message ─────────────────────────────────────────────────


def _make_stdin(messages: list[str]) -> io.StringIO:
    """Build a stdin buffer containing one or more Content-Length framed messages."""
    buf = io.StringIO()
    for msg in messages:
        body = msg if isinstance(msg, str) else json.dumps(msg)
        buf.write(f"Content-Length: {len(body)}\r\n\r\n{body}")
    buf.seek(0)
    return buf


class TestReadMcpMessage:
    def test_valid_message(self):
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"})
        old_stdin = sys.stdin
        sys.stdin = _make_stdin([body])
        try:
            result = mcp_server._read_mcp_message()
            assert result == {"jsonrpc": "2.0", "id": 1, "method": "initialize"}
        finally:
            sys.stdin = old_stdin

    def test_eof_raises_eoferror(self):
        """When stdin is exhausted, _read_mcp_message must raise EOFError
        so main() can break — NOT return None (which would cause an
        infinite tight loop)."""
        old_stdin = sys.stdin
        sys.stdin = io.StringIO("")  # immediate EOF
        try:
            with pytest.raises(EOFError):
                mcp_server._read_mcp_message()
        finally:
            sys.stdin = old_stdin

    def test_malformed_content_length_returns_none(self, capsys):
        old_stdin = sys.stdin
        sys.stdin = io.StringIO("Content-Length: abc\r\n\r\n{}")
        try:
            result = mcp_server._read_mcp_message()
            assert result is None
        finally:
            sys.stdin = old_stdin
        captured = capsys.readouterr()
        assert "malformed Content-Length" in captured.err

    def test_malformed_json_returns_none(self, capsys):
        old_stdin = sys.stdin
        sys.stdin = _make_stdin(["not valid json"])
        try:
            result = mcp_server._read_mcp_message()
            assert result is None
        finally:
            sys.stdin = old_stdin
        captured = capsys.readouterr()
        assert "malformed JSON" in captured.err

    def test_missing_content_length_returns_none(self):
        old_stdin = sys.stdin
        sys.stdin = io.StringIO("Some-Header: value\r\n\r\n")
        try:
            result = mcp_server._read_mcp_message()
            assert result is None
        finally:
            sys.stdin = old_stdin

    def test_multiple_messages_sequential(self):
        msg1 = json.dumps({"id": 1, "method": "initialize"})
        msg2 = json.dumps({"id": 2, "method": "tools/list"})
        old_stdin = sys.stdin
        sys.stdin = _make_stdin([msg1, msg2])
        try:
            r1 = mcp_server._read_mcp_message()
            r2 = mcp_server._read_mcp_message()
            assert r1["id"] == 1
            assert r2["id"] == 2
        finally:
            sys.stdin = old_stdin


# ── handle_request ────────────────────────────────────────────────────


class TestHandleRequest:
    def test_initialize(self):
        resp = mcp_server.handle_request({"jsonrpc": "2.0", "id": 1, "method": "initialize"})
        assert resp["id"] == 1
        assert resp["result"]["serverInfo"]["name"] == "cubesandbox-mcp"

    def test_tools_list(self):
        resp = mcp_server.handle_request({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        tool_names = [t["name"] for t in resp["result"]["tools"]]
        assert "sandbox_run_command" in tool_names
        assert "sandbox_run_code" in tool_names
        assert "sandbox_write_file" in tool_names
        assert "sandbox_read_file" in tool_names
        assert "sandbox_reset" in tool_names

    def test_unknown_method(self):
        resp = mcp_server.handle_request({"jsonrpc": "2.0", "id": 3, "method": "foo/bar"})
        assert "error" in resp
        assert resp["error"]["code"] == -32601

    def test_notifications_initialized_returns_none(self):
        resp = mcp_server.handle_request({"method": "notifications/initialized"})
        assert resp is None

    @patch("mcp_server.get_sandbox")
    def test_sandbox_run_command(self, mock_get):
        mock_sb = MagicMock()
        mock_result = MagicMock(exit_code=0, stdout="hello\n", stderr="")
        mock_sb.commands.run.return_value = mock_result
        mock_get.return_value = mock_sb

        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 10,
            "method": "tools/call",
            "params": {"name": "sandbox_run_command", "arguments": {"command": "echo hello"}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "hello" in text

    @patch("mcp_server.get_sandbox")
    def test_sandbox_run_code(self, mock_get):
        mock_sb = MagicMock()
        mock_result = MagicMock(exit_code=0, stdout="42\n", stderr="")
        mock_sb.commands.run.return_value = mock_result
        mock_get.return_value = mock_sb

        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 11,
            "method": "tools/call",
            "params": {"name": "sandbox_run_code", "arguments": {"code": "print(1+1)"}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "42" in text

    @patch("mcp_server.get_sandbox")
    def test_sandbox_write_file(self, mock_get):
        mock_sb = MagicMock()
        mock_get.return_value = mock_sb

        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 12,
            "method": "tools/call",
            "params": {"name": "sandbox_write_file",
                       "arguments": {"path": "/tmp/x.py", "content": "print(1)"}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "Written" in text
        mock_sb.files.write.assert_called_once_with("/tmp/x.py", "print(1)")

    @patch("mcp_server.get_sandbox")
    def test_sandbox_read_file(self, mock_get):
        mock_sb = MagicMock()
        mock_sb.files.read.return_value = "file contents"
        mock_get.return_value = mock_sb

        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 13,
            "method": "tools/call",
            "params": {"name": "sandbox_read_file", "arguments": {"path": "/tmp/x.py"}},
        })
        text = resp["result"]["content"][0]["text"]
        assert text == "file contents"

    @patch("mcp_server.get_sandbox")
    def test_sandbox_reset(self, mock_get):
        mock_sb = MagicMock()
        mcp_server._sandbox = mock_sb  # pretend a sandbox exists

        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 14,
            "method": "tools/call",
            "params": {"name": "sandbox_reset", "arguments": {}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "destroyed" in text.lower()
        mock_sb.kill.assert_called_once()
        assert mcp_server._sandbox is None

    @patch("mcp_server.get_sandbox")
    def test_unknown_tool_returns_error_text(self, mock_get):
        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 15,
            "method": "tools/call",
            "params": {"name": "nonexistent", "arguments": {}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "Unknown tool" in text

    @patch("mcp_server.get_sandbox")
    def test_exception_logged(self, mock_get, capsys):
        """handle_request must log the traceback, not silently swallow it."""
        mock_get.side_effect = RuntimeError("boom")
        resp = mcp_server.handle_request({
            "jsonrpc": "2.0", "id": 16,
            "method": "tools/call",
            "params": {"name": "sandbox_run_command", "arguments": {"command": "ls"}},
        })
        text = resp["result"]["content"][0]["text"]
        assert "Error" in text
        captured = capsys.readouterr()
        assert "Traceback" in captured.err


# ── get_sandbox / _cleanup_sandbox ────────────────────────────────────


class TestSandboxLifecycle:
    def setup_method(self):
        mcp_server._sandbox = None

    def teardown_method(self):
        mcp_server._sandbox = None

    @patch("mcp_server.atexit")
    @patch("e2b_code_interpreter.Sandbox")
    def test_creates_sandbox_and_registers_cleanup(self, mock_sb_cls, mock_atexit):
        mock_sb = MagicMock()
        mock_sb_cls.create.return_value = mock_sb

        sb = mcp_server.get_sandbox(timeout=600)
        assert sb is mock_sb
        mock_sb_cls.create.assert_called_once()
        # atexit.register must be called so the sandbox is killed on exit
        mock_atexit.register.assert_called_once_with(mcp_server._cleanup_sandbox)

    @patch("mcp_server.atexit")
    @patch("e2b_code_interpreter.Sandbox")
    def test_ttl_refreshed_on_reuse(self, mock_sb_cls, mock_atexit):
        mock_sb = MagicMock()
        mock_sb_cls.create.return_value = mock_sb

        # First call creates the sandbox
        mcp_server.get_sandbox(timeout=600)
        # Second call should refresh TTL, not create a new one
        mcp_server.get_sandbox(timeout=900)
        mock_sb.set_timeout.assert_called_once_with(900)
        assert mock_sb_cls.create.call_count == 1

    @patch("mcp_server.atexit")
    @patch("e2b_code_interpreter.Sandbox")
    def test_recreates_on_ttl_refresh_failure(self, mock_sb_cls, mock_atexit):
        mock_sb = MagicMock()
        mock_sb.set_timeout.side_effect = RuntimeError("expired")
        mock_sb_cls.create.return_value = mock_sb

        mcp_server.get_sandbox(timeout=600)
        assert mock_sb_cls.create.call_count == 1
        # Second call: set_timeout fails → create a new sandbox
        mcp_server.get_sandbox(timeout=600)
        assert mock_sb_cls.create.call_count == 2

    def test_cleanup_kills_sandbox(self):
        mock_sb = MagicMock()
        mcp_server._sandbox = mock_sb
        mcp_server._cleanup_sandbox()
        mock_sb.kill.assert_called_once()
        assert mcp_server._sandbox is None

    def test_cleanup_noop_when_no_sandbox(self):
        mcp_server._sandbox = None
        mcp_server._cleanup_sandbox()  # must not raise
        assert mcp_server._sandbox is None

    def test_cleanup_swallows_kill_errors(self):
        mock_sb = MagicMock()
        mock_sb.kill.side_effect = RuntimeError("already dead")
        mcp_server._sandbox = mock_sb
        mcp_server._cleanup_sandbox()  # must not raise
        assert mcp_server._sandbox is None


# ── main() integration ────────────────────────────────────────────────


class TestMain:
    def test_eof_exits_cleanly(self):
        """main() must break on EOF, not spin forever."""
        old_stdin, old_stdout = sys.stdin, sys.stdout
        sys.stdin = io.StringIO("")  # immediate EOF
        sys.stdout = io.StringIO()
        try:
            mcp_server.main()  # should return normally
        finally:
            sys.stdin, sys.stdout = old_stdin, old_stdout

    @patch("mcp_server.handle_request")
    def test_valid_request_produces_response(self, mock_handle):
        mock_handle.return_value = {"jsonrpc": "2.0", "id": 1, "result": {"ok": True}}
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"})
        old_stdin, old_stdout = sys.stdin, sys.stdout
        sys.stdin = _make_stdin([body])
        out_buf = io.StringIO()
        sys.stdout = out_buf
        try:
            mcp_server.main()
        finally:
            sys.stdin, sys.stdout = old_stdin, old_stdout
        output = out_buf.getvalue()
        assert "Content-Length:" in output
        parsed = json.loads(output.split("\r\n\r\n", 1)[1])
        assert parsed["result"]["ok"] is True
