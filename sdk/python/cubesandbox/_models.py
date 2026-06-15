# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json as jsonlib
from dataclasses import dataclass, field
from typing import Any, Iterable


@dataclass
class Logs:
    stdout: list[str] = field(default_factory=list)
    stderr: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, list[str]]:
        return {"stdout": self.stdout, "stderr": self.stderr}

    def to_json(self) -> str:
        return jsonlib.dumps(self.to_dict())


@dataclass
class ExecutionError:
    name: str
    value: str
    traceback: str = ""

    def __init__(self, name: str, value: str, traceback: str | list[str] | None = "", **_: Any):
        self.name = name
        self.value = value
        if isinstance(traceback, list):
            self.traceback = "\n".join(traceback)
        else:
            self.traceback = traceback or ""

    def to_dict(self) -> dict[str, str]:
        return {"name": self.name, "value": self.value, "traceback": self.traceback}

    def to_json(self) -> str:
        return jsonlib.dumps(self.to_dict())


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
    data: dict | None = None
    chart: Any | None = None
    is_main_result: bool = False
    extra: dict | None = None

    def __init__(
        self,
        text: str | None = None,
        html: str | None = None,
        markdown: str | None = None,
        svg: str | None = None,
        png: str | None = None,
        jpeg: str | None = None,
        pdf: str | None = None,
        latex: str | None = None,
        json: dict | None = None,
        json_data: dict | None = None,
        javascript: str | None = None,
        data: dict | None = None,
        chart: Any | None = None,
        is_main_result: bool = False,
        extra: dict | None = None,
        **_: Any,
    ):
        self.text = text
        self.html = html
        self.markdown = markdown
        self.svg = svg
        self.png = png
        self.jpeg = jpeg
        self.pdf = pdf
        self.latex = latex
        self.json = json if json is not None else json_data
        self.javascript = javascript
        self.data = data
        self.chart = chart
        self.is_main_result = is_main_result
        self.extra = extra

    def __getitem__(self, item: str) -> Any:
        return getattr(self, item)

    @property
    def json_data(self) -> dict | None:
        """Backward-compatible alias for E2B's ``json`` field."""
        return self.json

    @json_data.setter
    def json_data(self, value: dict | None) -> None:
        self.json = value

    def formats(self) -> Iterable[str]:
        formats: list[str] = []
        for key in (
            "text",
            "html",
            "markdown",
            "svg",
            "png",
            "jpeg",
            "pdf",
            "latex",
            "json",
            "javascript",
            "data",
            "chart",
        ):
            if getattr(self, key):
                formats.append(key)
        if self.extra:
            formats.extend(self.extra.keys())
        return formats

    def __str__(self) -> str:
        return self.__repr__()

    def __repr__(self) -> str:
        if self.text:
            return f"Result({self.text})"
        return "Result(Formats: " + ", ".join(self.formats()) + ")"

    def _repr_html_(self) -> str | None:
        return self.html

    def _repr_markdown_(self) -> str | None:
        return self.markdown

    def _repr_svg_(self) -> str | None:
        return self.svg

    def _repr_png_(self) -> str | None:
        return self.png

    def _repr_jpeg_(self) -> str | None:
        return self.jpeg

    def _repr_pdf_(self) -> str | None:
        return self.pdf

    def _repr_latex_(self) -> str | None:
        return self.latex

    def _repr_json_(self) -> dict | None:
        return self.json

    def _repr_javascript_(self) -> str | None:
        return self.javascript


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
        return f"Execution(Results: {self.results}, Logs: {self.logs}, Error: {self.error})"

    def to_json(self) -> str:
        data = {
            "results": _serialize_results(self.results),
            "logs": self.logs.to_dict(),
            "error": self.error.to_dict() if self.error else None,
        }
        return jsonlib.dumps(data)


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


@dataclass(init=False)
class OutputMessage:
    line: str
    timestamp: int | str = ""
    error: bool = False

    def __init__(
        self,
        line: str | None = None,
        timestamp: int | str = "",
        error: bool = False,
        *,
        text: str | None = None,
        is_stderr: bool | None = None,
    ):
        self.line = line if line is not None else (text or "")
        self.timestamp = timestamp
        self.error = error if is_stderr is None else is_stderr

    @property
    def text(self) -> str:
        """Backward-compatible alias for E2B's ``line`` field."""
        return self.line

    @property
    def is_stderr(self) -> bool:
        """Backward-compatible alias for E2B's ``error`` field."""
        return self.error

    def __str__(self) -> str:
        return self.line


def _serialize_results(results: list[Result]) -> list[dict[str, Any]]:
    serialized = []
    for result in results:
        item: dict[str, Any] = {}
        for key in result.formats():
            value = result[key]
            if key == "chart" and hasattr(value, "to_dict"):
                value = value.to_dict()
            item[key] = value
        item["text"] = result.text
        serialized.append(item)
    return serialized
