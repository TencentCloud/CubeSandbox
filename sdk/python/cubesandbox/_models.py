# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class Logs:
    stdout: list[str] = field(default_factory=list)
    stderr: list[str] = field(default_factory=list)


@dataclass
class ExecutionError:
    name: str
    value: str
    traceback: list[str] = field(default_factory=list)


@dataclass
class Result:
    text: str | None = None
    html: str | None = None
    markdown: str | None = None
    svg: str | None = None
    png: str | None = None
    jpeg: str | None = None
    pdf: str | None = None
    latex: str | None = None
    json: dict | None = None
    javascript: str | None = None
    is_main_result: bool = False
    extra: dict | None = None


@dataclass
class Execution:
    results: list[Result] = field(default_factory=list)
    logs: Logs = field(default_factory=Logs)
    error: ExecutionError | None = None
    execution_count: int | None = None

    @property
    def text(self) -> str | None:
        """Text of the main result (last expression value)."""
        for r in self.results:
            if r.is_main_result:
                return r.text
        return None

    def __repr__(self) -> str:
        return f"Execution(text={self.text!r}, error={self.error})"


@dataclass
class SnapshotInfo:
    """Metadata returned by snapshot-related APIs."""
    snapshot_id: str
    names: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "SnapshotInfo":
        return cls(
            snapshot_id=data.get("snapshotID", ""),
            names=data.get("names") or [],
        )


@dataclass
class OutputMessage:
    text: str
    timestamp: str = ""
    is_stderr: bool = False