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
  - Refuse shell-operator / expansion characters (``;|&`$`` / newlines),
    including variable expansion via ``$``

Out of scope (stack separately):
  - Guest confinement for an *allowlisted* binary (``cat /etc/passwd`` still
    runs if ``cat`` is allowed — prefer ``cube-tool`` so the guest re-checks
    the profile; raw allowlisted binaries remain a least-privilege concern)
  - Network egress (use ``allow_internet_access`` / CIDR policies)
  - Interpreters: default set excludes them; ``enable_code_execution=True``
    is an explicit privilege escalation (adds ``CODE_EXECUTION_BINARIES``)
  - Growing the allowlist: pass ``extra_binaries`` only with
    ``allow_unsafe_allowlist_extension=True`` (not a silent default)
"""

from __future__ import annotations

import shlex
from typing import Iterable

DEFAULT_ALLOWED_BINARIES: frozenset[str] = frozenset(
    {
        # Guest wrapper shipped by this example's Dockerfile (preferred path).
        "cube-tool",
        # Bare toolbox names (same as /etc/cube-sandbox/tool-profile.txt).
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
# Intentionally bare ``python3`` only — versioned names like ``python3.11`` stay
# denied unless added via extra_binaries + allow_unsafe_allowlist_extension.

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
    # Conservative: scan the raw string before shlex, so quoted metas
    # (e.g. ``echo 'safe; string'``) are also refused.
    return any(ch in command for ch in _SHELL_META_CHARS)


def _split_argv(command: str) -> list[str] | None:
    # Force POSIX splitting: this gate models Linux MicroVM argv0 checks even
    # if the controlling host Python happens to run on Windows.
    try:
        return shlex.split(command, posix=True)
    except ValueError:
        return None


def _resolve_allowed(
    *,
    enable_code_execution: bool,
    extra_binaries: Iterable[str] | None,
    allow_unsafe_allowlist_extension: bool,
) -> frozenset[str]:
    base = DEFAULT_ALLOWED_BINARIES
    if extra_binaries:
        extras = frozenset(extra_binaries)
        if extras and not allow_unsafe_allowlist_extension:
            raise ValueError(
                "extra_binaries requires allow_unsafe_allowlist_extension=True "
                "(refusing silent allowlist growth)"
            )
        base = base | extras
    if enable_code_execution:
        return base | CODE_EXECUTION_BINARIES
    return base


def is_allowlisted(
    command: str,
    *,
    enable_code_execution: bool = False,
    extra_binaries: Iterable[str] | None = None,
    allow_unsafe_allowlist_extension: bool = False,
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
    # ASCII `/` and `\` only — Unicode slash homoglyphs are out of scope for
    # this demo gate (same class of residual as undocumented shell metas).
    # Also refuse relative argv0 (``./echo``, ``../echo``). After posix
    # ``shlex``, a Windows-style ``..\echo`` may collapse to ``..echo``;
    # leading ``.`` still marks it as path-like.
    if "/" in binary or "\\" in binary or binary.startswith("."):
        return False
    allowed = _resolve_allowed(
        enable_code_execution=enable_code_execution,
        extra_binaries=extra_binaries,
        allow_unsafe_allowlist_extension=allow_unsafe_allowlist_extension,
    )
    return binary in allowed


def assert_allowlisted(
    command: str,
    *,
    enable_code_execution: bool = False,
    extra_binaries: Iterable[str] | None = None,
    allow_unsafe_allowlist_extension: bool = False,
) -> str:
    """Return command if allowlisted; otherwise raise AllowlistDenied."""
    if not is_allowlisted(
        command,
        enable_code_execution=enable_code_execution,
        extra_binaries=extra_binaries,
        allow_unsafe_allowlist_extension=allow_unsafe_allowlist_extension,
    ):
        if _has_shell_meta(command):
            raise AllowlistDenied(
                "command contains shell metacharacters "
                f"(host gate refuses shell chaining): {command!r}"
            )
        parts = _split_argv(command)
        if parts is None:
            raise AllowlistDenied(f"command could not be parsed: {command!r}")
        if not parts:
            raise AllowlistDenied(f"command is empty: {command!r}")
        binary = parts[0]
        if "/" in binary or "\\" in binary or binary.startswith("."):
            raise AllowlistDenied(
                f"path-style argv0 refused: {binary!r} (full: {command!r})"
            )
        raise AllowlistDenied(
            f"command not on tool allowlist: {binary!r} (full: {command!r})"
        )
    return command


def coerce_exit_code(value: object, *, what: str = "command") -> int:
    """Return int(exit_code) or raise RuntimeError preserving conversion context."""
    try:
        return int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError) as err:
        raise RuntimeError(f"{what} non-numeric exit_code: {value!r}") from err


def exit_code_from_exc(exc: BaseException) -> int:
    """Numeric exit_code from an SDK command failure, else re-raise ``exc``."""
    if not hasattr(exc, "exit_code"):
        raise exc
    return coerce_exit_code(exc.exit_code, what="command failure")
