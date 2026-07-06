# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Tests for cubesandbox_rewrite.py — the PreToolUse hook.

Security-critical: verifies that the sandbox-escape vulnerability
(multi-line newline injection) and the full-path recursion issue are
fixed, and that command rewriting is correct.
"""

from __future__ import annotations

import io
import json
import sys

import pytest

import cubesandbox_rewrite as hook

EXEC_BIN = hook.EXEC_BIN


# ── _already_sandboxed ────────────────────────────────────────────────


class TestAlreadySandboxed:
    """The _already_sandboxed() guard must be injection-proof."""

    @pytest.mark.parametrize("cmd", [
        EXEC_BIN,
        f"{EXEC_BIN} --session abc 'npm test'",
        f"{EXEC_BIN} --session s --mount /tmp 'ls'",
    ])
    def test_short_name_recognized(self, cmd):
        assert hook._already_sandboxed(cmd) is True

    @pytest.mark.parametrize("cmd", [
        "/home/user/.local/bin/cubesandbox-exec 'npm test'",
        "/usr/local/bin/cubesandbox-exec --session x 'git status'",
        "./cubesandbox-exec 'echo hi'",
    ])
    def test_full_path_recognized(self, cmd):
        """Full-path invocations must also be detected to prevent recursion."""
        assert hook._already_sandboxed(cmd) is True

    @pytest.mark.parametrize("cmd", [
        "npm test",
        "git status",
        "python3 -c 'print(1+1)'",
        "ls -la",
        "",
        "   ",
    ])
    def test_normal_commands_not_matched(self, cmd):
        assert hook._already_sandboxed(cmd) is False

    # ── Security: newline injection ────────────────────────────────────

    @pytest.mark.parametrize("cmd", [
        # A literal newline after the binary name must NOT match — the
        # second line would execute on the host, bypassing the sandbox.
        f"{EXEC_BIN}\nrm -rf /",
        f"{EXEC_BIN} \nrm -rf /",
        # Carriage-return variant
        f"{EXEC_BIN}\rrm -rf /",
        # CRLF
        f"{EXEC_BIN}\r\necho pwned",
    ])
    def test_newline_injection_blocked(self, cmd):
        """The critical security fix: multi-line commands must NOT be
        mistaken for already-sandboxed calls."""
        assert hook._already_sandboxed(cmd) is False

    def test_tab_is_not_injection(self):
        """A tab is ordinary whitespace in bash — ``cubesandbox-exec\\trm``
        is a single command (cubesandbox-exec with arg 'rm'), so it IS
        already sandboxed and should return True."""
        assert hook._already_sandboxed(f"{EXEC_BIN}\trm -rf /") is True

    def test_quoted_newline_in_argument(self):
        """A newline *inside* a quoted argument is legitimate and should
        not cause a false negative — shlex handles this correctly."""
        # shlex.split keeps the newline inside the single-quoted argument,
        # so tokens[0] is still EXEC_BIN.
        cmd = f"{EXEC_BIN} 'echo hello\\nworld'"
        assert hook._already_sandboxed(cmd) is True

    @pytest.mark.parametrize("cmd", [
        f"{EXEC_BIN}extra 'cmd'",          # prefix match, not exact
        f"not-{EXEC_BIN} 'cmd'",            # different binary entirely
        f"{EXEC_BIN}_backup 'cmd'",         # suffix variant
    ])
    def test_lookalike_names_not_matched(self, cmd):
        assert hook._already_sandboxed(cmd) is False

    def test_malformed_shlex(self):
        """Unbalanced quotes → shlex.error → return False (safe default)."""
        assert hook._already_sandboxed(f"{EXEC_BIN} 'unbalanced") is False

    def test_empty_string(self):
        assert hook._already_sandboxed("") is False

    def test_only_whitespace(self):
        assert hook._already_sandboxed("   \t\n  ") is False


# ── main() — end-to-end hook behaviour ────────────────────────────────


def _run_hook(payload: dict) -> tuple[str | None, int]:
    """Feed *payload* to hook.main() via stdin, return (stdout, exit_code)."""
    stdin_buf = io.StringIO(json.dumps(payload))
    stdout_buf = io.StringIO()
    old_stdin, old_stdout = sys.stdin, sys.stdout
    sys.stdin, sys.stdout = stdin_buf, stdout_buf
    try:
        hook.main()
        exit_code = 0
    except SystemExit as e:
        exit_code = e.code if isinstance(e.code, int) else 0
    finally:
        sys.stdin, sys.stdout = old_stdin, old_stdout
    out = stdout_buf.getvalue()
    return (out if out else None), exit_code


class TestMainRewrite:
    def test_basic_bash_command_rewritten(self):
        payload = {
            "session_id": "abc123",
            "cwd": "/home/user/project",
            "tool_name": "Bash",
            "tool_input": {"command": "npm test", "timeout": 120000},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is not None
        result = json.loads(out)
        assert result["hookSpecificOutput"]["permissionDecision"] == "allow"
        rewritten = result["hookSpecificOutput"]["updatedInput"]["command"]
        assert rewritten.startswith(f"{EXEC_BIN} ")
        assert "--session" in rewritten and "abc123" in rewritten
        assert "--mount" in rewritten and "/home/user/project" in rewritten
        assert "--timeout" in rewritten and "120.000" in rewritten
        assert "npm test" in rewritten

    def test_non_bash_tool_passes_through(self):
        payload = {
            "session_id": "abc123",
            "tool_name": "Read",
            "tool_input": {"file_path": "/etc/hosts"},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is None  # hook exits silently for non-Bash tools

    def test_already_sandboxed_passes_through(self):
        payload = {
            "session_id": "abc123",
            "tool_name": "Bash",
            "tool_input": {"command": f"{EXEC_BIN} --session x 'ls'"},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is None  # no rewrite — let it execute as-is

    def test_malformed_json_exits_silently(self):
        stdin_buf = io.StringIO("not json {{{")
        old_stdin = sys.stdin
        sys.stdin = stdin_buf
        try:
            with pytest.raises(SystemExit) as exc_info:
                hook.main()
            assert exc_info.value.code == 0
        finally:
            sys.stdin = old_stdin

    def test_missing_command_passes_through(self):
        payload = {
            "session_id": "abc123",
            "tool_name": "Bash",
            "tool_input": {},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is None

    def test_missing_session_defaults(self):
        payload = {
            "tool_name": "Bash",
            "tool_input": {"command": "echo hi"},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is not None
        result = json.loads(out)
        rewritten = result["hookSpecificOutput"]["updatedInput"]["command"]
        assert "--session" in rewritten and "default" in rewritten

    def test_no_cwd_omits_mount(self):
        payload = {
            "session_id": "abc123",
            "tool_name": "Bash",
            "tool_input": {"command": "echo hi"},
        }
        out, code = _run_hook(payload)
        assert code == 0
        result = json.loads(out)
        rewritten = result["hookSpecificOutput"]["updatedInput"]["command"]
        assert "--mount" not in rewritten

    def test_zero_timeout_omitted(self):
        payload = {
            "session_id": "abc123",
            "tool_name": "Bash",
            "tool_input": {"command": "echo hi", "timeout": 0},
        }
        out, code = _run_hook(payload)
        assert code == 0
        result = json.loads(out)
        rewritten = result["hookSpecificOutput"]["updatedInput"]["command"]
        assert "--timeout" not in rewritten

    def test_newline_injection_not_rewritten(self):
        """A command that tries to inject a newline after cubesandbox-exec
        must be treated as a *normal* command and wrapped (not skipped)."""
        malicious = f"{EXEC_BIN}\nrm -rf /"
        payload = {
            "session_id": "abc123",
            "tool_name": "Bash",
            "tool_input": {"command": malicious},
        }
        out, code = _run_hook(payload)
        assert code == 0
        assert out is not None
        result = json.loads(out)
        rewritten = result["hookSpecificOutput"]["updatedInput"]["command"]
        # The malicious command should be wrapped inside cubesandbox-exec
        # as a single quoted argument, not passed through as-is.
        assert rewritten.startswith(f"{EXEC_BIN} --session")
