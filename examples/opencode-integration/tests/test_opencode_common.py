# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for _opencode_common.py — shared sandbox helpers.

Mirrors the structure of test_common.py in the Claude Code integration
so the two examples can be reviewed side-by-side. Differences:

- The OpenCode helper accepts both ``envs=`` (e2b-compatible) and ``env=``
  (older SDKs); the test pins the fallback path so a regression in either
  signature does not silently mask a TypeError.
- We duck-type on ``exit_code`` rather than catching
  ``CommandExitException`` because the two supported SDKs disagree on
  whether non-zero exits raise. Test ensures the helper never lets an
  exception escape the call boundary.
"""

from __future__ import annotations

from unittest.mock import MagicMock, Mock

from e2b.sandbox.commands.command_handle import CommandExitException

import _opencode_common


def make_result(exit_code: int = 0, stdout: str = "", stderr: str = "") -> Mock:
    """Create a mock command result with the three attrs our helpers read."""
    r = Mock()
    r.exit_code = exit_code
    r.stdout = stdout
    r.stderr = stderr
    return r


# ── run_command ──────────────────────────────────────────────────────────


class TestRunCommand:
    """Tests for _opencode_common.run_command() — exec-channel wrapper."""

    def test_returns_result_on_success(self) -> None:
        sandbox = Mock()
        expected = make_result(0, "ok", "")
        sandbox.commands.run.return_value = expected
        result = _opencode_common.run_command(sandbox, "echo hello")
        assert result is expected

    def test_returns_result_on_non_zero_exit(self) -> None:
        # run_command swallows CommandExitException and returns it as the result,
        # so callers can uniformly branch on exit_code without catching everywhere.
        sandbox = Mock()
        exc = CommandExitException(
            stderr="err", stdout="out", exit_code=1, error="failure"
        )
        sandbox.commands.run.side_effect = exc
        result = _opencode_common.run_command(sandbox, "false")
        assert result is exc

    def test_default_user_is_user(self) -> None:
        # The e2b / CubeSandbox exec channel rejects unknown usernames, so
        # we hard-code ``user`` (uid 1000) as the default non-root account.
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result()
        _opencode_common.run_command(sandbox, "ls")
        sandbox.commands.run.assert_called_once_with("ls", user="user")

    def test_passes_timeout_and_cwd(self) -> None:
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result()
        _opencode_common.run_command(sandbox, "whoami", cwd="/tmp", timeout=42)
        kwargs = sandbox.commands.run.call_args.kwargs
        assert kwargs["cwd"] == "/tmp"
        assert kwargs["timeout"] == 42

    def test_falls_back_to_env_kwarg_for_legacy_sdk(self) -> None:
        # Older SDKs name the parameter ``env`` instead of ``envs``. Only
        # retry for that specific signature mismatch — other TypeErrors
        # (wrong-type command, etc.) must propagate so real bugs surface.
        sandbox = Mock()
        sandbox.commands.run.side_effect = [
            TypeError("unexpected keyword argument 'envs'"),
            make_result(0, "", ""),
        ]
        _opencode_common.run_command(sandbox, "printenv", envs={"K": "v"})
        # Second call must use the renamed ``env`` kwarg.
        kwargs = sandbox.commands.run.call_args_list[1].kwargs
        assert "envs" not in kwargs
        assert kwargs["env"] == {"K": "v"}

    def test_non_envs_typeerror_propagates(self) -> None:
        # A TypeError that has nothing to do with the envs→env rename must
        # not be silently masked.
        sandbox = Mock()
        sandbox.commands.run.side_effect = TypeError("bad arg shape")
        with __import__("pytest").raises(TypeError, match="bad arg shape"):
            _opencode_common.run_command(sandbox, "x")

    def test_stream_mode_attaches_on_stdout_and_stderr(self) -> None:
        sandbox = Mock()
        sandbox.commands.run.return_value = make_result()
        _opencode_common.run_command(sandbox, "tail -f", stream=True)
        kwargs = sandbox.commands.run.call_args.kwargs
        assert callable(kwargs["on_stdout"])
        assert callable(kwargs["on_stderr"])