#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""CubeSandbox MCP Server for CodeBuddy.

Exposes a small set of tools that let any MCP-capable client (Claude
Desktop, Cursor, Windsurf, VS Code, etc.) execute untrusted code or shell
commands inside an isolated CubeSandbox MicroVM instead of on the host.
The same backend (`sandbox_exec.py`) handles the actual work; this file
just adapts it to the Model Context Protocol's newline-delimited JSON-RPC
transport on stdio.

Add to your MCP client's config (Claude Desktop example shown):

    {
      "mcpServers": {
        "cubesandbox-codebuddy": {
          "command": "python3",
          "args": [
            "/abs/path/to/CubeSandbox/examples/codebuddy-integration/mcp_server.py"
          ],
          "env": {
            "CUBE_API_URL": "http://<cube-host>:3000",
            "CUBE_API_KEY": "<api-key>",
            "CUBE_TEMPLATE_ID": "<template-id>"
          }
        }
      }
    }

Security notes:
- The server executes commands inside an isolated MicroVM, limiting blast radius.
- File paths are validated syntactically (normpath only) to prevent obvious
  path traversal. The sandbox's isolated namespace is the authoritative boundary.
- No authentication is required when accessed via local MCP client config.
- Ensure MCP client configurations are properly secured in production.
"""

from __future__ import annotations

import atexit
import json
import logging
import os
import shlex
import sys
import threading
import traceback
from typing import Any

from dotenv import load_dotenv

logger = logging.getLogger(__name__)

load_dotenv()

# --- Configuration -----------------------------------------------------------

# CUBE_API_URL / CUBE_API_KEY are the canonical names (documented in .env.example).
# E2B_API_URL / E2B_API_KEY are accepted as legacy aliases.
E2B_API_URL = os.getenv("CUBE_API_URL") or os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("CUBE_API_KEY") or os.getenv("E2B_API_KEY", "e2b_000000")
TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")

# Security limits
MAX_TIMEOUT = 300  # 5 minutes maximum
MAX_CODE_LENGTH = 100_000  # 100KB
MAX_CONTENT_LENGTH = 1_000_000  # 1MB
MAX_MESSAGE_LENGTH = 1_000_000  # 1MB per JSON-RPC line on stdio
MAX_COMMAND_LENGTH = 65536  # 64KB

# Allowed path prefixes for file operations (sandbox-side).
# In a typical setup, the sandbox VM only has access to /workspace.
_ALLOWED_PATH_PREFIXES = ("/workspace", "/tmp", "/home/user")

_sandbox: Any = None
_sandbox_lock = threading.Lock()


def _get_sandbox(timeout: int = 600):
    """Lazy-create a sandbox and reuse it across tool calls until the process exits."""
    global _sandbox
    with _sandbox_lock:
        if _sandbox is None:
            from e2b_code_interpreter import Sandbox
            if not TEMPLATE_ID:
                raise RuntimeError("CUBE_TEMPLATE_ID is not set")
            _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
        else:
            try:
                _sandbox.set_timeout(timeout)
            except Exception:
                try:
                    _sandbox.kill()
                except Exception:
                    # Orphaned sandbox — set_timeout and kill both failed.
                    # Log for operator visibility; the sandbox will remain alive
                    # on the cluster until its TTL expires.
                    logger.warning(
                        "Failed to set timeout and kill sandbox; "
                        "sandbox may be orphaned (id=%s)", getattr(_sandbox, "id", "unknown")
                    )
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


def _validate_path(path: str) -> str | None:
    """Validate a path is syntactically within allowed prefixes.

    Returns None if valid, error message otherwise.

    Note: This validation is purely syntactic (uses os.path.normpath, not
    os.path.realpath). It checks that the normalized path starts with an
    allowed prefix. The actual file operations run inside the sandbox's
    isolated namespace, where the sandbox's own filesystem policies are
    the authoritative security boundary. realpath is not used because it
    would resolve against the host's symlinks, which may differ from the
    sandbox's private namespace.
    """
    if not isinstance(path, str):
        return "path must be a string"
    if not path:
        return "path cannot be empty"
    # Normalize path syntactically and check prefix
    try:
        normalized = os.path.normpath(path)
    except (ValueError, OSError):
        return "invalid path"
    for prefix in _ALLOWED_PATH_PREFIXES:
        # Use strict prefix check with os.sep to avoid /workspace-stuff matching /workspace
        if normalized.startswith(prefix + os.sep) or normalized == prefix:
            return None
    return f"path must be within {', '.join(_ALLOWED_PATH_PREFIXES)}"


def _validate_timeout(timeout: int | None) -> int:
    """Validate and bound timeout value."""
    if timeout is None:
        return MAX_TIMEOUT
    if not isinstance(timeout, int):
        return MAX_TIMEOUT
    return min(max(1, timeout), MAX_TIMEOUT)


def _validate_string_length(value: str, max_length: int, field_name: str) -> str | None:
    """Validate string length. Returns None if valid, error message otherwise."""
    if not isinstance(value, str):
        return f"{field_name} must be a string"
    if len(value) > max_length:
        return f"{field_name} exceeds maximum length of {max_length} bytes"
    return None


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
            "stderr": str(exc)[:2048],  # Truncate to prevent info leak
        }
    except Exception as exc:
        return {
            "exit_code": 1,
            "stdout": "",
            "stderr": f"execution error: {type(exc).__name__}",
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
                    "description": "Execution timeout in seconds (default 120, max 300)",
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
                    "description": "Execution timeout in seconds (default 120, max 300)",
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
                    "description": "Absolute path inside the sandbox (e.g. /workspace/script.py)",
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
                    "name": "cubesandbox-codebuddy-mcp",
                    "version": "1.0.0",
                },
            },
        }

    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": req_id, "result": {"tools": TOOLS}}

    if method == "tools/call":
        params = request.get("params")
        if not isinstance(params, dict):
            return {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "content": [{"type": "text", "text": "Invalid arguments: missing params"}],
                    "isError": True,
                },
            }
        tool_name = params.get("name", "")
        arguments = params.get("arguments", {})
        text = ""
        is_error = False
        try:
            if tool_name == "sandbox_run_code":
                code = arguments.get("code", "")
                timeout = _validate_timeout(arguments.get("timeout"))
                err = _validate_string_length(code, MAX_CODE_LENGTH, "code")
                if err:
                    raise ValueError(err)
                if not code.strip():
                    raise ValueError("code cannot be empty")
                result = run_command(f"python3 -c {shlex.quote(code)}", timeout=timeout)
                text = _format_command_result(result)
                is_error = result["exit_code"] != 0

            elif tool_name == "sandbox_run_command":
                cmd = arguments.get("command", "")
                timeout = _validate_timeout(arguments.get("timeout"))
                err = _validate_string_length(cmd, MAX_COMMAND_LENGTH, "command")
                if err:
                    raise ValueError(err)
                if not cmd.strip():
                    raise ValueError("command cannot be empty")
                result = run_command(cmd, timeout=timeout)
                text = _format_command_result(result)
                is_error = result["exit_code"] != 0

            elif tool_name == "sandbox_write_file":
                path = arguments.get("path", "")
                content = arguments.get("content", "")
                err = _validate_path(path)
                if err:
                    raise ValueError(err)
                err = _validate_string_length(content, MAX_CONTENT_LENGTH, "content")
                if err:
                    raise ValueError(err)
                _get_sandbox().files.write(path, content)
                text = f"Written {len(content)} bytes to {path}"

            elif tool_name == "sandbox_read_file":
                path = arguments.get("path", "")
                err = _validate_path(path)
                if err:
                    raise ValueError(err)
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

        except (ValueError, TypeError, KeyError) as exc:
            text = f"Invalid arguments: {exc}"
            is_error = True
        except (OSError, RuntimeError) as exc:
            # Expected errors from sandbox operations (network, file I/O, timeout).
            traceback.print_exc(file=sys.stderr)
            text = f"Error: {exc}"
            is_error = True
        except (KeyboardInterrupt, SystemExit):
            raise  # propagate without wrapping — daemon stop is not an error
        except BaseException as exc:
            # Fatal: MemoryError, RecursionError, etc. — log and propagate so
            # the daemon can restart cleanly rather than silently continuing.
            traceback.print_exc(file=sys.stderr)
            raise

        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [{"type": "text", "text": text}],
                "isError": is_error,
            },
        }

    if method == "notifications/initialized":
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
    lines return None so the loop can skip them without hanging.  Each line is
    bounded by MAX_MESSAGE_LENGTH to prevent a malicious client from exhausting
    the server's memory with a multi-gigabyte payload.
    """
    line = sys.stdin.readline()
    if not line:
        raise EOFError()
    if len(line) > MAX_MESSAGE_LENGTH:
        return None
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
