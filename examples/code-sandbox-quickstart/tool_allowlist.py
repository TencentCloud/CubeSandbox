# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-side argv allowlist gate for agent tool commands.

Not a kernel/enforcement layer inside the MicroVM. Models how an agent host
should refuse non-allowlisted tool invocations before Sandbox.commands.run().

Default set is tool-level only. ``enable_code_execution=True`` is an explicit
privilege escalation (adds ``CODE_EXECUTION_BINARIES``).
"""

from __future__ import annotations

import shlex
from typing import Iterable

DEFAULT_ALLOWED_BINARIES: frozenset[str] = frozenset(
    {
        "echo",
        "uname",
        "pwd",
        "ls",
        "cat",
        "head",
        "wc",
        "sha256sum",
    }
)

CODE_EXECUTION_BINARIES: frozenset[str] = frozenset({"python3"})


class AllowlistDenied(PermissionError):
    """Raised when a command is not on the host-side tool allowlist."""


def _split_argv(command: str) -> list[str] | None:
    try:
        return shlex.split(command)
    except ValueError:
        return None


def _resolve_allowed(
    allowed_binaries: Iterable[str] | None,
    *,
    enable_code_execution: bool,
) -> frozenset[str]:
    base = (
        frozenset(allowed_binaries)
        if allowed_binaries is not None
        else DEFAULT_ALLOWED_BINARIES
    )
    if enable_code_execution:
        return base | CODE_EXECUTION_BINARIES
    return base


def is_allowlisted(
    command: str,
    allowed_binaries: Iterable[str] | None = None,
    *,
    enable_code_execution: bool = False,
) -> bool:
    """True if the first argv token is on the effective allowlist."""
    parts = _split_argv(command)
    if not parts:
        return False
    binary = parts[0]
    # Path-style first tokens rejected. shlex is POSIX-mode on this Linux demo.
    if "/" in binary or "\\" in binary:
        return False
    allowed = _resolve_allowed(
        allowed_binaries, enable_code_execution=enable_code_execution
    )
    return binary in allowed


def assert_allowlisted(
    command: str,
    allowed_binaries: Iterable[str] | None = None,
    *,
    enable_code_execution: bool = False,
) -> str:
    """Return command if allowlisted; otherwise raise AllowlistDenied."""
    if not is_allowlisted(
        command,
        allowed_binaries,
        enable_code_execution=enable_code_execution,
    ):
        parts = _split_argv(command)
        binary = parts[0] if parts else ""
        raise AllowlistDenied(
            f"command not on tool allowlist: {binary!r} (full: {command!r})"
        )
    return command
