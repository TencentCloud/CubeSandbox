# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared sandbox command helpers for the CodeBuddy example scripts.

Kept SDK-agnostic (duck-typed on ``sandbox.commands.run`` and the result's
attributes) so the same helpers work with both the e2b-compatible SDK used by
``run_codebuddy.py`` / ``resume_codebuddy.py`` and the native ``cubesandbox``
SDK used by ``network_policy.py``.
"""

from __future__ import annotations

import argparse
import sys
from collections.abc import Callable
from typing import Any

from e2b.sandbox.commands.command_handle import CommandExitException


def positive_int(value: str) -> int:
    """argparse type that rejects zero and negative integers.

    Both the CLI value and the env-var fallback default flow through this
    function so passing ``--exec-timeout 0`` fails the same way as setting
    ``CODEBUDDY_AGENT_EXEC_TIMEOUT=0`` in the environment.
    """
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise argparse.ArgumentTypeError(f"expected an integer, got {value!r}") from None
    if parsed <= 0:
        raise argparse.ArgumentTypeError(
            f"expected a positive integer, got {value}"
        )
    return parsed


def stream_writer(stream) -> Callable[[object], None]:
    """Create a callback that writes chunks to a stream with error handling.

    Errors during write/flush are logged to stderr rather than raised,
    preventing stream exceptions from aborting the command execution.
    """
    def write(chunk: object) -> None:
        try:
            text = getattr(chunk, "line", chunk)
            stream.write(str(text))
            stream.flush()
        except OSError:
            pass  # Broken pipe / closed stream — command likely terminated
        except Exception:
            print(f"[stream writer error: {type(chunk).__name__}]", file=sys.stderr)

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    stream: bool = False,
    user: str = "user",
):
    # The e2b / CubeSandbox exec channel rejects unknown usernames with
    # "invalid username: '<x>'", so we cannot pass ``codebuddy`` here even
    # though the image's USER directive drops privileges. ``user`` (uid 1000)
    # is the default non-root account the base image ships; pairing that with
    # the image-level USER means any containerized tool runs unprivileged, while
    # the exec channel stays within the SDK-accepted allow-list.
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        kwargs["on_stdout"] = stream_writer(sys.stdout)
        kwargs["on_stderr"] = stream_writer(sys.stderr)

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        # Older SDKs name the parameter ``env`` instead of ``envs``.  The retry
        # only fires when the error message mentions "envs" explicitly — other
        # TypeErrors (e.g. bad timeout value) are re-raised so real bugs are
        # not masked.
        if "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)
    except CommandExitException as exc:
        # Some SDKs raise on non-zero exits instead of returning a result.
        # Swallow it here so every caller can uniformly branch on exit_code
        # without catching at every call site.
        return exc


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
