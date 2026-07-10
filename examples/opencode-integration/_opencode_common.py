# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared OpenCode command, result, session, and cleanup helpers."""

from __future__ import annotations

import json
import shlex
import sys
from collections.abc import Collection, Iterator
from typing import Any

from env_utils import redact_secrets


class CommandExecutionError(RuntimeError):
    """Raised when a command inside the sandbox exits unsuccessfully."""


def opencode_command(
    prompt: str,
    *,
    model: str,
    workspace: str = "/workspace",
    title: str | None = None,
    session_id: str | None = None,
    continue_last: bool = False,
    auto: bool = True,
    json_format: bool = True,
) -> str:
    """Build a quoted, non-interactive ``opencode run`` invocation."""
    if session_id and continue_last:
        raise ValueError("session_id and continue_last are mutually exclusive")
    if not prompt.strip():
        raise ValueError("prompt must not be empty")
    if not model.strip() or "/" not in model:
        raise ValueError("model must use OpenCode's provider/model form")

    args = ["opencode", "run", "--model", model, "--dir", workspace]
    if title:
        args.extend(["--title", title])
    if session_id:
        args.extend(["--session", session_id])
    elif continue_last:
        args.append("--continue")
    if auto:
        args.append("--auto")
    if json_format:
        args.extend(["--format", "json"])
    args.append(prompt)
    return shlex.join(args)


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    user: str = "root",
):
    """Execute through the SDK while supporting its ``envs``/``env`` aliases."""
    kwargs: dict[str, object] = {"user": user}
    if cwd is not None:
        kwargs["cwd"] = cwd
    if timeout is not None:
        kwargs["timeout"] = timeout
    if envs:
        kwargs["envs"] = envs

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def redacted_result_output(
    result: object,
    *,
    secrets: Collection[str] | None = None,
) -> tuple[str, str]:
    stdout = redact_secrets(getattr(result, "stdout", ""), secrets)
    stderr = redact_secrets(getattr(result, "stderr", ""), secrets)
    return stdout, stderr


def ensure_success(
    result: object,
    action: str,
    *,
    secrets: Collection[str] | None = None,
) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code in (None, 0):
        return
    stdout, stderr = redacted_result_output(result, secrets=secrets)
    raise CommandExecutionError(
        f"Failed to {action} (exit {exit_code}).\n"
        f"STDOUT:\n{stdout}\nSTDERR:\n{stderr}"
    )


def extract_session_id(output: str, *, title: str | None = None) -> str:
    """Extract a session ID from OpenCode JSON events or ``session list`` JSON.

    The pinned CLI is contract-tested by this example's tests. Matching a
    unique title is preferred for session-list output so an older session is
    never resumed accidentally.
    """
    candidates: list[tuple[str, str | None]] = []
    for document in _json_documents(output):
        candidates.extend(_session_candidates(document))

    if title is not None:
        for session_id, candidate_title in candidates:
            if candidate_title == title:
                return session_id
        raise ValueError(f"No OpenCode session found with title {title!r}")

    if candidates:
        return candidates[0][0]
    raise ValueError("OpenCode output did not contain a session ID")


def _json_documents(output: str) -> Iterator[object]:
    stripped = output.strip()
    if not stripped:
        return
    try:
        yield json.loads(stripped)
        return
    except (TypeError, ValueError):
        pass

    for line in output.splitlines():
        if not line.strip():
            continue
        try:
            yield json.loads(line)
        except (TypeError, ValueError):
            continue


def _session_candidates(value: object) -> list[tuple[str, str | None]]:
    found: list[tuple[str, str | None]] = []
    if isinstance(value, dict):
        title_value = value.get("title")
        title = title_value if isinstance(title_value, str) else None
        for key in ("sessionID", "sessionId", "session_id"):
            session_id = value.get(key)
            if isinstance(session_id, str) and session_id:
                found.append((session_id, title))
        generic_id = value.get("id")
        value_type = value.get("type")
        if (
            isinstance(generic_id, str)
            and generic_id
            and (title is not None or value_type == "session")
        ):
            found.append((generic_id, title))
        for nested in value.values():
            found.extend(_session_candidates(nested))
    elif isinstance(value, list):
        for item in value:
            found.extend(_session_candidates(item))
    return found


def sandbox_identifier(sandbox: Any) -> str:
    return str(getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown")))


def safe_kill(sandbox: Any, *, label: str = "sandbox") -> Exception | None:
    """Best-effort cleanup that reports, rather than masks, an earlier error."""
    if sandbox is None:
        return None
    try:
        sandbox.kill()
    except Exception as exc:  # noqa: BLE001 - cleanup must preserve root cause
        print(
            f"Warning: failed to kill {label} {sandbox_identifier(sandbox)}: {exc}",
            file=sys.stderr,
        )
        return exc
    return None
