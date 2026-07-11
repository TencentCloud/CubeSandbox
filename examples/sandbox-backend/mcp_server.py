#!/usr/bin/env python3
"""
CubeSandbox MCP Server for Claude Code.

Provides tools that let Claude Code automatically execute untrusted code
in isolated MicroVM sandboxes instead of directly on the host.

Add to ~/.claude/mcp.json or project .claude/mcp.json:

{
  "mcpServers": {
    "cubesandbox": {
      "command": "python3",
      "args": [
        "/path/to/CubeSandbox/examples/sandbox-backend/mcp_server.py"
      ],
      "env": {
        "E2B_API_URL": "http://127.0.0.1:3000",
        "E2B_API_KEY": "e2b_000000",
        "CUBE_TEMPLATE_ID": "tpl-c703537d5106496790d44702"
      }
    }
  }
}
"""

import atexit
import json
import os
import shlex
import sys
import traceback
# ── Configuration ──────────────────────────────────────────────────────
E2B_API_URL = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("E2B_API_KEY", "e2b_000000")
TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")
SANDBOX_TTL = int(os.getenv("CUBE_SANDBOX_TIMEOUT", "600"))

_sandbox = None


def _cleanup_sandbox():
    """Kill the cached sandbox on process exit to avoid orphans."""
    global _sandbox
    if _sandbox is not None:
        try:
            _sandbox.kill()
        except Exception:
            pass
        _sandbox = None


def get_sandbox(timeout=SANDBOX_TTL):
    """Lazy-create and reuse a sandbox, refreshing its TTL on each call.

    Without TTL refresh the sandbox expires after ``SANDBOX_TTL`` seconds
    of inactivity, causing the next tool call to fail with
    ``SandboxNotFoundError``.  An ``atexit`` handler ensures the sandbox
    is killed when the MCP server process exits.
    """
    global _sandbox
    if _sandbox is None:
        from e2b_code_interpreter import Sandbox
        _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
        atexit.register(_cleanup_sandbox)
    else:
        try:
            _sandbox.set_timeout(timeout)
        except Exception:
            # Sandbox may have expired or been killed — create a fresh one.
            from e2b_code_interpreter import Sandbox
            _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
    return _sandbox


def run_command(cmd, timeout=120):
    """Run a command inside the sandbox."""
    from e2b.sandbox.commands.command_handle import CommandExitException
    try:
        result = get_sandbox().commands.run(cmd, timeout=timeout)
        return {"exit_code": result.exit_code, "stdout": result.stdout.strip(), "stderr": result.stderr.strip()}
    except CommandExitException as e:
        return {"exit_code": getattr(e, "exit_code", 1), "stdout": "", "stderr": str(e)}


# ── MCP Tool Definitions ──────────────────────────────────────────────

TOOLS = [
    {
        "name": "sandbox_run_code",
        "description": "Execute Python code in an isolated CubeSandbox MicroVM. Use this for running untrusted code, testing generated scripts, or installing packages safely.",
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
        "description": "Execute a shell command in an isolated CubeSandbox MicroVM. Use this to safely test shell commands, explore file systems, or run build tools without affecting the host.",
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
                    "description": "Absolute path inside the sandbox (e.g., /tmp/script.py)",
                },
                "content": {
                    "type": "string",
                    "description": "File content to write",
                },
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
                "path": {
                    "type": "string",
                    "description": "Absolute path inside the sandbox",
                },
            },
            "required": ["path"],
        },
    },
    {
        "name": "sandbox_reset",
        "description": "Destroy the current sandbox and create a fresh one. Use this between unrelated tasks to get a clean environment.",
        "inputSchema": {
            "type": "object",
            "properties": {},
        },
    },
]


# ── MCP Protocol Handler ──────────────────────────────────────────────

def handle_request(request):
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
                    "name": "cubesandbox-mcp",
                    "version": "1.0.0",
                },
            },
        }

    elif method == "tools/list":
        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {"tools": TOOLS},
        }

    elif method == "tools/call":
        tool_name = request["params"]["name"]
        arguments = request["params"].get("arguments", {})

        try:
            if tool_name == "sandbox_run_code":
                code = arguments["code"]
                timeout = arguments.get("timeout", 120)
                result = run_command(f"python3 -c {shlex.quote(code)}", timeout=timeout)
                text = result["stdout"] or result["stderr"] or "(no output)"

            elif tool_name == "sandbox_run_command":
                cmd = arguments["command"]
                timeout = arguments.get("timeout", 120)
                result = run_command(cmd, timeout=timeout)
                text = result["stdout"] or result["stderr"] or "(no output)"

            elif tool_name == "sandbox_write_file":
                path = arguments["path"]
                content = arguments["content"]
                get_sandbox().files.write(path, content)
                text = f"Written {len(content)} bytes to {path}"

            elif tool_name == "sandbox_read_file":
                path = arguments["path"]
                content = get_sandbox().files.read(path)
                text = content if isinstance(content, str) else content.decode("utf-8", errors="replace")

            elif tool_name == "sandbox_reset":
                global _sandbox
                if _sandbox:
                    _sandbox.kill()
                    _sandbox = None
                text = "Sandbox destroyed. A new one will be created on next use."

            else:
                text = f"Unknown tool: {tool_name}"

        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            text = f"Error: {e}"

        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [{"type": "text", "text": text}],
            },
        }

    elif method == "notifications/initialized":
        return None  # No response for notifications

    else:
        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "error": {"code": -32601, "message": f"Method not found: {method}"},
        }


# ── Main Loop ─────────────────────────────────────────────────────────

def _read_mcp_message():
    """Read one MCP message from stdin using Content-Length framing.

    Returns the parsed JSON request, ``None`` for a malformed (skippable)
    message, or raises ``EOFError`` when stdin is exhausted so the caller
    can break out of the read loop instead of spinning.
    """
    content_length = None
    read_any = False
    for line in sys.stdin:
        read_any = True
        line = line.rstrip("\r\n")
        if not line:
            break  # empty line = end of headers
        if line.startswith("Content-Length:"):
            try:
                content_length = int(line.split(":", 1)[1].strip())
            except ValueError:
                print(f"[cubesandbox-mcp] warning: malformed Content-Length header: {line!r}", file=sys.stderr)
                return None
    if not read_any:
        raise EOFError  # stdin exhausted — client disconnected
    if content_length is None:
        return None
    body = sys.stdin.read(content_length)
    try:
        return json.loads(body)
    except json.JSONDecodeError as e:
        print(f"[cubesandbox-mcp] warning: malformed JSON body: {e}", file=sys.stderr)
        return None


def main():
    """Run the MCP server on stdio (JSON-RPC)."""
    from dotenv import load_dotenv
    load_dotenv()
    while True:
        try:
            request = _read_mcp_message()
        except EOFError:
            break
        if request is None:
            continue
        response = handle_request(request)
        if response is not None:
            body = json.dumps(response)
            sys.stdout.write(f"Content-Length: {len(body)}\r\n\r\n{body}")
            sys.stdout.flush()


if __name__ == "__main__":
    main()
