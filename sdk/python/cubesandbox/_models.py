# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from dataclasses import dataclass, field
from typing import List, Optional


@dataclass
class Logs:
    stdout: List[str] = field(default_factory=list)
    stderr: List[str] = field(default_factory=list)


@dataclass
class ExecutionError:
    name: str
    value: str
    traceback: List[str] = field(default_factory=list)


@dataclass
class Result:
    text: Optional[str] = None
    html: Optional[str] = None
    markdown: Optional[str] = None
    svg: Optional[str] = None
    png: Optional[str] = None
    jpeg: Optional[str] = None
    pdf: Optional[str] = None
    latex: Optional[str] = None
    json_data: Optional[dict] = None
    javascript: Optional[str] = None
    is_main_result: bool = False
    extra: Optional[dict] = None


@dataclass
class Execution:
    results: List[Result] = field(default_factory=list)
    logs: Logs = field(default_factory=Logs)
    error: Optional[ExecutionError] = None
    execution_count: Optional[int] = None

    @property
    def text(self) -> Optional[str]:
        """Text of the main result (last expression value)."""
        for r in self.results:
            if r.is_main_result:
                return r.text
        return None

    def __repr__(self) -> str:
        return f"Execution(text={self.text!r}, error={self.error})"


@dataclass
class OutputMessage:
    text: str
    timestamp: str = ""
    is_stderr: bool = False
