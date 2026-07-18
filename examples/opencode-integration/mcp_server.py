#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""CubeSandbox MCP Server for OpenCode.

Exposes a small set of tools that let any MCP-capable client (Claude
Desktop, Cursor, Windsurf, VS Code, etc.) execute untrusted code or shell
commands inside an isolated CubeSandbox MicroVM instead of on the host.
The same backend (`sandbox_exec.py`) handles the actual work; this file
just adapts it to the Model Context Protocol's newline-delimited JSON-RPC
transport on stdio.

Add to your MCP client's config (Claude Desktop example shown):

    {
      "mcpServers": {
        "cubesandbox-opencode": {
          "command": "python3",
          "args": [
            "/abs/path/to/CubeSandbox/examples/opencode-integration/mcp_server.py"
          ],
          "env": {
            "E2B_API_URL": "http://<cube-host>:3000",
            "E2B_API_KEY": "<api-key>",
            "CUBE_TEMPLATE_ID": "<template-id>"
          }
        }
      }
    }
"""

from __future__ import annotations

import atexit
import json
import os
import shlex
import sys
from typing import Any

from dotenv import load_dotenv

load_dotenv()

# --- Configuration -----------------------------------------------------------

E2B_API_URL = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("E2B_API_KEY", "e2b_000000")
TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")

_sandbox: Any = None


def _get_sandbox(timeout: int = 600):
    """Lazy-create a sandbox and reuse it across tool calls until the process exits."""
    global _sandbox
    if _sandbox is None:
        from e2b_code_interpreter import Sandbox
        if not TEMPLATE_ID:
            raise RuntimeError("CUBE_TEMPLATE_ID is not set")
        _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
    else:
        try:
            _sandbox.set_timeout(timeout)
        except Exception:
            # The sandbox expired or was killed under us — try to clean up
            # whatever remains before allocating a fresh one.
            try:
                _sandbox.kill()
            except Exception:
                pass
            from e2b_code_interpreter import Sandbox
            _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
    return _sandbox


def _cleanup_sandbox() -> None:
    """Destroy the cached sandbox when the MCP process exits."""
    global _sandbox
    sandbox, _sandbox = _sandbox, None
    if sandbox is not None:
        try:
            sandbox.kill()
        except Exception as exc:
            print(f"Failed to clean up sandbox: {exc}", file=sys.stderr)


atexit.register(_cleanup_sandbox)


def run_command(cmd: str, timeout: int = 120) -> dict[str, Any]:
    """Run a shell command inside the sandbox, capturing exit code / stdout / stderr."""
    from e2b.sandbox.commands.command_handle import CommandExitException
    try:
        result = _get_sandbox().commands.run(cmd, timeout=timeout)
        return {
            "exit_code": result.exit_code,
            "stdout": result.stdout.strip() if result.stdout else "",
            "stderr": result.stderr.strip() if result.stderr else "",
        }
    except CommandExitException as exc:
        return {
            "exit_code": getattr(exc, "exit_code", 1),
            "stdout": "",
            "stderr": str(exc),
        }


def _format_command_result(result: dict[str, Any]) -> str:
    """Render a command result for an MCP tool response.

    The exit code is always included so the LLM can branch on success /
    failure without having to parse free-form text. stdout / stderr are
    rendered only when non-empty.
    """
    sections = [f"exit_code: {result['exit_code']}"]
    if result["stdout"]:
        sections.append(f"stdout:\n{result['stdout']}")
    if result["stderr"]:
        sections.append(f"stderr:\n{result['stderr']}")
    if len(sections) == 1:
        sections.append("(no output)")
    return "\n".join(sections)


# --- Tool definitions --------------------------------------------------------

TOOLS: list[dict[str, Any]] = [
    {
        "name": "sandbox_run_code",
        "description": (
            "Execute Python code in an isolated CubeSandbox MicroVM. Use this for "
            "running untrusted code, testing generated scripts, or installing "
            "packages safely."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "code": {
                    "type": "string",
                    "description": "Python code to execute in the sandbox",
                },
                "timeout": {
                    "type": "integer",
                    "description": "Execution timeout in seconds (default 120)",
                    "default": 120,
                },
            },
            "required": ["code"],
        },
    },
    {
        "name": "sandbox_run_command",
        "description": (
            "Execute a shell command in an isolated CubeSandbox MicroVM. Use this "
            "to safely test shell commands, explore file systems, or run build "
            "tools without affecting the host."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "Shell command to execute in the sandbox",
                },
                "timeout": {
                    "type": "integer",
                    "description": "Execution timeout in seconds (default 120)",
                    "default": 120,
                },
            },
            "required": ["command"],
        },
    },
    {
        "name": "sandbox_write_file",
        "description": "Write content to a file inside the sandbox.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "Absolute path inside the sandbox (e.g. /tmp/script.py)",
                },
                "content": {"type": "string", "description": "File content to write"},
            },
            "required": ["path", "content"],
        },
    },
    {
        "name": "sandbox_read_file",
        "description": "Read a file's content from inside the sandbox.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Absolute path inside the sandbox"},
            },
            "required": ["path"],
        },
    },
    {
        "name": "sandbox_reset",
        "description": (
            "Destroy the current sandbox and create a fresh one. Use between "
            "unrelated tasks to get a clean environment."
        ),
        "inputSchema": {"type": "object", "properties": {}},
    },
]


# --- JSON-RPC handler --------------------------------------------------------

def handle_request(request: dict[str, Any]) -> dict[str, Any] | None:
    method = request.get("method", "")
    req_id = request.get("id")

    if method == "initialize":
        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {
                    "name": "cubesandbox-opencode-mcp",
                    "version": "1.0.0",
                },
            },
        }

    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": req_id, "result": {"tools": TOOLS}}

    if method == "tools/call":
        tool_name = request["params"]["name"]
        arguments = request["params"].get("arguments", {})
        text = ""
        is_error = False
        try:
            if tool_name == "sandbox_run_code":
                code = arguments["code"]
                timeout = arguments.get("timeout", 120)
                result = run_command(f"python3 -c {shlex.quote(code)}", timeout=timeout)
                text = _format_command_result(result)
                is_error = result["exit_code"] != 0

            elif tool_name == "sandbox_run_command":
                cmd = arguments["command"]
                timeout = arguments.get("timeout", 120)
                result = run_command(cmd, timeout=timeout)
                text = _format_command_result(result)
                is_error = result["exit_code"] != 0

            elif tool_name == "sandbox_write_file":
                path = arguments["path"]
                content = arguments["content"]
                _get_sandbox().files.write(path, content)
                text = f"Written {len(content)} bytes to {path}"

            elif tool_name == "sandbox_read_file":
                path = arguments["path"]
                content = _get_sandbox().files.read(path)
                text = content if isinstance(content, str) else content.decode(
                    "utf-8", errors="replace"
                )

            elif tool_name == "sandbox_reset":
                _cleanup_sandbox()
                text = "Sandbox destroyed. A new one will be created on next use."

            else:
                text = f"Unknown tool: {tool_name}"
                is_error = True

        except Exception as exc:
            import traceback
            traceback.print_exc(file=sys.stderr)
            text = f"Error: {exc}"
            is_error = True

        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [{"type": "text", "text": text}],
                "isError": is_error,
            },
        }

    if method == "notifications/initialized":
        # Notifications get no response; returning None lets the main loop
        # skip the write rather than emit a JSON-RPC reply for them.
        return None

    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "error": {"code": -32601, "message": f"Method not found: {method}"},
    }


# --- Main loop ---------------------------------------------------------------

def _read_mcp_message() -> dict[str, Any] | None:
    """Read one newline-delimited JSON-RPC message from MCP stdio.

    Raises EOFError when stdin is exhausted (client disconnected). Malformed
    lines return None so the loop can skip them without hanging.
    """
    line = sys.stdin.readline()
    if not line:
        raise EOFError()
    try:
        return json.loads(line)
    except json.JSONDecodeError:
        return None


def main() -> None:
    """Run the MCP server on stdio (newline-delimited JSON-RPC)."""
    try:
        while True:
            try:
                request = _read_mcp_message()
            except EOFError:
                break
            if request is None:
                continue
            response = handle_request(request)
            if response is not None:
                sys.stdout.write(json.dumps(response) + "\n")
                sys.stdout.flush()
    finally:
        _cleanup_sandbox()


if __name__ == "__main__":
    main()