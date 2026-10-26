# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class CommandResult:
    stdout: str
    stderr: str
    exit_code: int


@dataclass(frozen=True)
class CodeResult:
    text: str
    stdout: list[str] = field(default_factory=list)
    stderr: list[str] = field(default_factory=list)
    error: Any | None = None


@dataclass(frozen=True)
class SandboxInfo:
    sandbox_id: str
    state: str | None = None
    raw: dict[str, Any] = field(default_factory=dict)


def first_present(data: dict[str, Any], *keys: str) -> Any | None:
    for key in keys:
        if key in data:
            return data[key]
    return None


def state_from_raw(raw: dict[str, Any]) -> str | None:
    for key in ("state", "State", "status", "Status"):
        value = raw.get(key)
        if value:
            return str(value)
    return None
