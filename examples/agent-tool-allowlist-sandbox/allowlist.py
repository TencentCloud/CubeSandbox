# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-side argv allowlist gate for agent tool commands.

This is intentionally NOT a kernel/enforcement layer inside the MicroVM.
It models how an agent host should refuse non-allowlisted tool invocations
before they reach Sandbox.commands.run().
"""

from __future__ import annotations

import shlex
from typing import Iterable, Sequence


# Default allowlist: exact binary names (first argv token) only.
# Keep this narrow — the point of the example is least privilege for agent tools.
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
        "python3",
    }
)


class AllowlistDenied(PermissionError):
    """Raised when a command is not on the host-side tool allowlist."""


def _first_token(command: str) -> str:
    parts = shlex.split(command)
    if not parts:
        raise AllowlistDenied("empty command")
    return parts[0]


def is_allowlisted(
    command: str,
    allowed_binaries: Iterable[str] | None = None,
) -> bool:
    allowed = frozenset(allowed_binaries) if allowed_binaries is not None else DEFAULT_ALLOWED_BINARIES
    binary = _first_token(command)
    # Reject path-style invocations that bypass simple name checks (e.g. /bin/bash).
    if "/" in binary or "\\" in binary:
        return False
    return binary in allowed


def assert_allowlisted(
    command: str,
    allowed_binaries: Sequence[str] | Iterable[str] | None = None,
) -> str:
    """Return the command if allowlisted; otherwise raise AllowlistDenied."""
    if not is_allowlisted(command, allowed_binaries):
        binary = _first_token(command)
        raise AllowlistDenied(
            f"command not on tool allowlist: {binary!r} (full: {command!r})"
        )
    return command
