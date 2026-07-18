# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for mcp_server.py — MCP protocol handling and sandbox lifecycle.

Mirrors the structure of test_mcp_server.py in the Claude Code integration
so the two examples can be reviewed side-by-side.
"""

from __future__ import annotations

import io
import json
import sys
from unittest.mock import Mock, patch

import pytest

import mcp_server


@pytest.fixture(autouse=True)
def reset_sandbox():
    """Reset the module-level _sandbox between tests."""
    old = mcp_server._sandbox
    mcp_server._sandbox = None
    yield
    mcp_server._sandbox = old


# ── _read_mcp_message ──────────────────────────────────────────────────


class TestReadMcpMessage:
    """Tests for mcp_server._read_mcp_message() — protocol message parsing."""

    def test_valid_message(self) -> None:
        body = json.dumps({"jsonrpc": "2.0", "method": "initialize", "id": 1})
        stdin = io.StringIO(body + "\n")
        with patch.object(sys, "stdin", stdin):
            msg = mcp_server._read_mcp_message()
        assert msg == {"jsonrpc": "2.0", "method": "initialize", "id": 1}

    def test_eof_raises_eoferror(self) -> None:
        # EOF now raises EOFError instead of returning None, which
        # previously caused main() to loop infinitely on disconnected clients.
        stdin = io.StringIO("")
        with patch.object(sys, "stdin", stdin):
            with pytest.raises(EOFError):
                mcp_server._read_mcp_message()

    def test_invalid_json_returns_none(self) -> None:
        stdin = io.StringIO("not json\n")
        with patch.object(sys, "stdin", stdin):
            msg = mcp_server._read_mcp_message()
        assert msg is None

    def test_blank_line_returns_none(self) -> None:
        stdin = io.StringIO("\n")
        with patch.object(sys, "stdin", stdin):
            msg = mcp_server._read_mcp_message()
        assert msg is None


# ── handle_request ─────────────────────────────────────────────────────


class TestHandleRequest:
    """Tests for mcp_server.handle_request() — JSON-RPC dispatch."""

    def test_initialize(self) -> None:
        resp = mcp_server.handle_request(
            {"jsonrpc": "2.0", "method": "initialize", "id": 1}
        )
        assert resp["id"] == 1
        assert resp["result"]["protocolVersion"] == "2024-11-05"
        assert "tools" in resp["result"]["capabilities"]

    def test_tools_list(self) -> None:
        resp = mcp_server.handle_request(
            {"jsonrpc": "2.0", "method": "tools/list", "id": 2}
        )
        assert resp["id"] == 2
        tools = resp["result"]["tools"]
        assert len(tools) > 0
        names = {t["name"] for t in tools}
        assert "sandbox_run_code" in names
        assert "sandbox_run_command" in names
        assert "sandbox_write_file" in names
        assert "sandbox_read_file" in names
        assert "sandbox_reset" in names

    def test_unknown_method_returns_error(self) -> None:
        resp = mcp_server.handle_request(
            {"jsonrpc": "2.0", "method": "unknown", "id": 3}
        )
        assert resp["id"] == 3
        assert "error" in resp
        assert resp["error"]["code"] == -32601

    def test_notifications_initialized_returns_none(self) -> None:
        resp = mcp_server.handle_request({"method": "notifications/initialized"})
        assert resp is None

    def test_tools_call_unknown_tool(self) -> None:
        resp = mcp_server.handle_request({
            "jsonrpc": "2.0",
            "method": "tools/call",
            "id": 5,
            "params": {"name": "nonexistent", "arguments": {}},
        })
        assert resp["id"] == 5
        assert "Unknown tool" in resp["result"]["content"][0]["text"]
        assert resp["result"]["isError"] is True

    def test_command_failure_preserves_exit_code_and_both_streams(self) -> None:
        result = {"exit_code": 7, "stdout": "partial output", "stderr": "failed"}
        with patch.object(mcp_server, "run_command", return_value=result):
            resp = mcp_server.handle_request({
                "jsonrpc": "2.0",
                "method": "tools/call",
                "id": 6,
                "params": {
                    "name": "sandbox_run_command",
                    "arguments": {"command": "false"},
                },
            })

        tool_result = resp["result"]
        text = tool_result["content"][0]["text"]
        assert tool_result["isError"] is True
        assert "exit_code: 7" in text
        assert "stdout:\npartial output" in text
        assert "stderr:\nfailed" in text

    def test_run_code_pipes_through_python(self) -> None:
        with patch.object(mcp_server, "run_command", return_value={
            "exit_code": 0, "stdout": "42", "stderr": ""
        }) as mock_run:
            mcp_server.handle_request({
                "jsonrpc": "2.0",
                "method": "tools/call",
                "id": 7,
                "params": {
                    "name": "sandbox_run_code",
                    "arguments": {"code": "print(42)"},
                },
            })
        first_arg = mock_run.call_args[0][0]
        assert first_arg.startswith("python3 -c ")
        assert "\nprint(42)" not in first_arg

    def test_write_file_reports_byte_count(self) -> None:
        with patch.object(mcp_server, "_get_sandbox") as mock_sb:
            mcp_server.handle_request({
                "jsonrpc": "2.0",
                "method": "tools/call",
                "id": 8,
                "params": {
                    "name": "sandbox_write_file",
                    "arguments": {"path": "/tmp/x", "content": "abc"},
                },
            })
        mock_sb.return_value.files.write.assert_called_once_with("/tmp/x", "abc")

    def test_reset_cleans_up_sandbox(self) -> None:
        with patch.object(mcp_server, "_cleanup_sandbox") as mock_cleanup:
            mcp_server.handle_request({
                "jsonrpc": "2.0",
                "method": "tools/call",
                "id": 9,
                "params": {"name": "sandbox_reset", "arguments": {}},
            })
        mock_cleanup.assert_called_once()


# ── _get_sandbox ───────────────────────────────────────────────────────


class TestGetSandbox:
    """Tests for mcp_server._get_sandbox() — sandbox lifecycle + TTL refresh."""

    def test_creates_sandbox_on_first_call(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("CUBE_TEMPLATE_ID", "tpl-test")
        # Reload module-level TEMPLATE_ID after patching env
        mcp_server.TEMPLATE_ID = "tpl-test"
        mock_instance = Mock()
        mock_instance.sandbox_id = "sb-new"
        with patch("e2b_code_interpreter.Sandbox") as mock_cls:
            mock_cls.create.return_value = mock_instance
            mcp_server._sandbox = None
            result = mcp_server._get_sandbox(timeout=600)
        assert result is mock_instance
        mock_cls.create.assert_called_once()

    def test_refreshes_ttl_on_subsequent_call(self) -> None:
        mock_instance = Mock()
        mcp_server._sandbox = mock_instance
        result = mcp_server._get_sandbox(timeout=600)
        assert result is mock_instance
        mock_instance.set_timeout.assert_called_once_with(600)

    def test_recreates_sandbox_if_set_timeout_fails(self) -> None:
        mock_instance = Mock()
        mock_instance.set_timeout.side_effect = Exception("expired")
        mcp_server._sandbox = mock_instance

        new_instance = Mock()
        with patch("e2b_code_interpreter.Sandbox") as mock_cls:
            mock_cls.create.return_value = new_instance
            result = mcp_server._get_sandbox()
        assert result is new_instance
        mock_instance.kill.assert_called_once()
        mock_cls.create.assert_called_once()

    def test_cleanup_kills_and_clears_cached_sandbox(self) -> None:
        mock_instance = Mock()
        mcp_server._sandbox = mock_instance
        mcp_server._cleanup_sandbox()
        mock_instance.kill.assert_called_once()
        assert mcp_server._sandbox is None


# ── main loop ──────────────────────────────────────────────────────────


def test_main_uses_newline_delimited_json_and_cleans_up() -> None:
    request = json.dumps({"jsonrpc": "2.0", "method": "tools/list", "id": 9})
    stdin = io.StringIO(request + "\n")
    stdout = io.StringIO()
    with (
        patch.object(sys, "stdin", stdin),
        patch.object(sys, "stdout", stdout),
        patch.object(mcp_server, "_cleanup_sandbox") as cleanup,
    ):
        mcp_server.main()

    lines = stdout.getvalue().splitlines()
    assert len(lines) == 1
    assert json.loads(lines[0])["id"] == 9
    cleanup.assert_called_once()


def test_main_exits_cleanly_on_eof() -> None:
    stdin = io.StringIO("")
    with (
        patch.object(sys, "stdin", stdin),
        patch.object(sys, "stdout", io.StringIO()),
        patch.object(mcp_server, "_cleanup_sandbox") as cleanup,
    ):
        mcp_server.main()
    cleanup.assert_called_once()
