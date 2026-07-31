# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json as jsonlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
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


class SandboxState(str, Enum):
    """E2B-compatible sandbox lifecycle state.

    Subclasses ``str`` so comparisons against plain strings (e.g.
    ``state == "running"``) keep working for existing callers.
    """

    RUNNING = "running"
    PAUSED = "paused"

    def __str__(self) -> str:
        return self.value

    @classmethod
    def _missing_(cls, value: object) -> SandboxState | None:
        if isinstance(value, str):
            for member in cls:
                if member.value == value.lower():
                    return member
        return None


def _parse_timestamp(value: Any) -> datetime | None:
    """Parse a CubeAPI timestamp into a timezone-aware ``datetime``.

    Accepts ISO-8601 strings (including a trailing ``Z``) and unix epoch
    seconds. Returns ``None`` when the value is missing or unparseable so a
    single malformed field never breaks ``get_info()``.
    """
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        try:
            return datetime.fromtimestamp(value, tz=timezone.utc)
        except (ValueError, OverflowError, OSError):
            return None
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return None
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        # CubeAPI timestamps often carry nanosecond precision (9 digits).
        # datetime.fromisoformat on Python <3.11 only accepts up to microseconds.
        if "." in text:
            head, frac_and_tz = text.split(".", 1)
            digits = ""
            tz = ""
            for i, ch in enumerate(frac_and_tz):
                if ch.isdigit():
                    digits += ch
                else:
                    tz = frac_and_tz[i:]
                    break
            if digits:
                text = f"{head}.{digits[:6]}{tz}"
        try:
            return datetime.fromisoformat(text)
        except ValueError:
            return None
    return None


def _normalize_state(value: Any) -> SandboxState | str | None:
    """Normalize a raw state string into :class:`SandboxState`.

    Unknown or missing values gracefully fall back to the raw value so new
    backend states never raise.
    """
    if value is None:
        return None
    if isinstance(value, SandboxState):
        return value
    if not isinstance(value, str):
        return str(value)
    try:
        return SandboxState(value)
    except ValueError:
        return value


@dataclass
class SandboxInfo(dict[str, Any]):
    """E2B-compatible sandbox metadata returned by :meth:`Sandbox.get_info`.

    Attribute access exposes E2B-style ``snake_case`` fields with typed
    values (``datetime`` timestamps, :class:`SandboxState` state). The object is
    also a real ``dict`` containing the raw CubeAPI JSON snapshot for backward
    compatibility, except that the sensitive ``envdAccessToken`` is excluded
    from iteration and serialization.

    Mutating the dict does not update the typed attributes, and mutating an
    attribute does not update the raw dict snapshot.
    """

    sandbox_id: str
    template_id: str
    sandbox_domain: str | None = None
    name: str | None = None
    metadata: dict[str, str] = field(default_factory=dict)
    started_at: datetime | None = None
    end_at: datetime | None = None
    state: SandboxState | str | None = None
    cpu_count: int | None = None
    memory_mb: int | None = None
    envd_version: str = ""
    _envd_access_token: str | None = field(default=None, repr=False)
    disk_size_mb: int | None = None

    def __post_init__(self) -> None:
        """Populate a CubeAPI-shaped dict for directly constructed objects."""
        raw: dict[str, Any] = {
            "sandboxID": self.sandbox_id,
            "templateID": self.template_id,
            "metadata": dict(self.metadata),
        }
        optional = {
            "domain": self.sandbox_domain,
            "alias": self.name,
            "startedAt": self.started_at.isoformat() if self.started_at else None,
            "endAt": self.end_at.isoformat() if self.end_at else None,
            "state": self.state.value if isinstance(self.state, SandboxState) else self.state,
            "cpuCount": self.cpu_count,
            "memoryMB": self.memory_mb,
            "envdVersion": self.envd_version,
            "diskSizeMB": self.disk_size_mb,
        }
        raw.update({key: value for key, value in optional.items() if value is not None})
        dict.__init__(self, raw)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> SandboxInfo:
        """Build typed attributes while preserving the exact raw API mapping."""
        info = cls(
            sandbox_id=data.get("sandboxID", ""),
            template_id=data.get("templateID", ""),
            sandbox_domain=data.get("domain"),
            name=data.get("alias"),
            metadata=data.get("metadata") or {},
            started_at=_parse_timestamp(data.get("startedAt")),
            end_at=_parse_timestamp(data.get("endAt")),
            state=_normalize_state(data.get("state")),
            cpu_count=data.get("cpuCount"),
            memory_mb=data.get("memoryMB"),
            envd_version=data.get("envdVersion", ""),
            _envd_access_token=data.get("envdAccessToken"),
            disk_size_mb=data.get("diskSizeMB"),
        )
        dict.clear(info)
        dict.update(info, {key: value for key, value in data.items() if key != "envdAccessToken"})
        return info

    def __missing__(self, key: str) -> Any:
        """Allow explicit legacy token lookup without serializing the token."""
        if key == "envdAccessToken" and self._envd_access_token is not None:
            return self._envd_access_token
        raise KeyError(key)

    def get(self, key: str, default: Any = None) -> Any:
        if key == "envdAccessToken":
            return self._envd_access_token if self._envd_access_token is not None else default
        return super().get(key, default)

    def to_dict(self) -> dict[str, Any]:
        """Return a serializable raw snapshot with sensitive tokens excluded."""
        return dict(self)


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
