# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-side argv allowlist gate for agent tool commands.

This is intentionally NOT a kernel/enforcement layer inside the MicroVM.
It models how an agent host should refuse non-allowlisted tool invocations
before they reach Sandbox.commands.run().

Capability note: the default set is tool-level only. Granting an interpreter
(``enable_code_execution=True`` / ``CODE_EXECUTION_BINARIES``) is a privilege
escalation to arbitrary guest code execution — not "just another binary".
"""

from __future__ import annotations

import shlex
from typing import Iterable


# Default: narrow tool binaries (first argv token only). No interpreters.
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

# Explicit capability escalation: arbitrary code execution inside the guest.
# Prefer enable_code_execution=True at the call site over silently unioning this.
CODE_EXECUTION_BINARIES: frozenset[str] = frozenset({"python3"})


class AllowlistDenied(PermissionError):
    """Raised when a command is not on the host-side tool allowlist."""


def _first_token(command: str) -> str:
    parts = shlex.split(command)
    if not parts:
        raise AllowlistDenied("empty command")
    return parts[0]


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
    """Return True if the first argv token is on the effective allowlist.

    Empty / whitespace-only commands return False (predicate contract).
    Path-style first tokens (``/`` or ``\\``) return False.
    """
    parts = shlex.split(command)
    if not parts:
        return False
    binary = parts[0]
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
    """Return the command if allowlisted; otherwise raise AllowlistDenied.

    Set ``enable_code_execution=True`` only when the caller intentionally
    grants guest arbitrary code execution (adds ``CODE_EXECUTION_BINARIES``).

    Parsing and path checks live in ``is_allowlisted`` so the two APIs cannot drift.
    """
    if not is_allowlisted(
        command,
        allowed_binaries,
        enable_code_execution=enable_code_execution,
    ):
        # Re-parse only for the error message (empty → AllowlistDenied("empty command")).
        binary = _first_token(command)
        raise AllowlistDenied(
            f"command not on tool allowlist: {binary!r} (full: {command!r})"
        )
    return command
