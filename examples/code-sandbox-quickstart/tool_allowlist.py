# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-side argv allowlist gate for agent tool commands.

Not a kernel/enforcement layer inside the MicroVM. Models how an agent host
should refuse non-allowlisted tool invocations before Sandbox.commands.run().

Threat model (host policy only)
-------------------------------
In scope:
  - Refuse tools whose first argv token is outside the allowlist
  - Refuse path-style first tokens (``/bin/echo``, ``..\\\\echo``)
  - Refuse shell-operator characters (``;|&`$`` / newlines) so
    ``echo ok; bash -c ...`` cannot sneak past a first-token-only check
    when the string later hits a shell

Out of scope (stack separately):
  - Guest confinement for an *allowlisted* binary (``cat /etc/passwd`` still
    runs if ``cat`` is allowed — use MicroVM isolation + least privilege)
  - Network egress (use ``allow_internet_access`` / CIDR policies)
  - Interpreters: default set excludes them; ``enable_code_execution=True``
    is an explicit privilege escalation (adds ``CODE_EXECUTION_BINARIES``)
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

# Shell chaining / expansion markers. Rejected up front so a naive
# first-token check cannot be bypassed via ``echo ok; bash -c ...``.
# Parentheses / redirects are not listed: tools like ``python3 -c 'print(1)'``
# need ``()``, and residual redirect risk belongs with guest least-privilege.
_SHELL_META_CHARS: frozenset[str] = frozenset(
    {
        ";",
        "|",
        "&",
        "`",
        "$",
        "\n",
        "\r",
    }
)


class AllowlistDenied(PermissionError):
    """Raised when a command is not on the host-side tool allowlist."""


def _has_shell_meta(command: str) -> bool:
    return any(ch in command for ch in _SHELL_META_CHARS)


def _split_argv(command: str) -> list[str] | None:
    # Force POSIX splitting: this gate models Linux MicroVM argv0 checks even
    # if the controlling host Python happens to run on Windows.
    try:
        return shlex.split(command, posix=True)
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
    """True if the command string is acceptable under the host gate."""
    if not command or not command.strip():
        return False
    if _has_shell_meta(command):
        return False
    parts = _split_argv(command)
    if not parts:
        return False
    binary = parts[0]
    # Path-style first tokens rejected (POSIX argv0 model for Linux guests).
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
        if _has_shell_meta(command):
            raise AllowlistDenied(
                "command contains shell metacharacters "
                f"(host gate refuses shell chaining): {command!r}"
            )
        parts = _split_argv(command)
        binary = parts[0] if parts else ""
        raise AllowlistDenied(
            f"command not on tool allowlist: {binary!r} (full: {command!r})"
        )
    return command
