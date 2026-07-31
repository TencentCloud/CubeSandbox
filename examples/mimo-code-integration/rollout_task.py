# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Load one bounded demonstration task for the speculative rollout pattern."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any

MAX_FIXTURE_FILES = 64
MAX_FIXTURE_BYTES = 1024 * 1024


def _required_string(payload: dict[str, Any], name: str) -> str:
    value = payload.get(name)
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"task field {name!r} must be a non-empty string")
    return value.strip()


def _positive_int(payload: dict[str, Any], name: str) -> int:
    value = payload.get(name)
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise ValueError(f"task field {name!r} must be a positive integer")
    return value


def _safe_relative_path(raw: str) -> str:
    path = PurePosixPath(raw)
    if (
        not raw
        or path.is_absolute()
        or path.as_posix() != raw
        or ".." in path.parts
        or any(character.isspace() for character in raw)
        or "\\" in raw
        or re.fullmatch(r"[A-Za-z0-9._/-]+", raw) is None
    ):
        raise ValueError(f"task path must be a normalized relative path: {raw!r}")
    return raw


@dataclass(frozen=True)
class RolloutTask:
    """Task-specific inputs consumed by the reusable rollout lifecycle."""

    name: str
    summary: str
    planning_instructions: str
    implementation_instructions: str
    test_command: str
    test_timeout_seconds: int
    allowed_paths: tuple[str, ...]
    strategies: tuple[tuple[str, str], ...]
    expect_baseline_failure: bool
    project_dir: Path

    def fixture_files(self) -> tuple[tuple[str, str], ...]:
        """Return a small, regular-file-only project fixture."""
        files: list[tuple[str, str]] = []
        total_bytes = 0
        for path in sorted(self.project_dir.rglob("*")):
            if path.is_symlink():
                raise ValueError(f"fixture symlinks are not allowed: {path}")
            if not path.is_file():
                continue
            relative_path = path.relative_to(self.project_dir)
            if ".git" in relative_path.parts:
                raise ValueError("task fixture must not contain Git metadata")
            if "__pycache__" in relative_path.parts or path.suffix == ".pyc":
                continue
            relative = _safe_relative_path(relative_path.as_posix())
            total_bytes += path.stat().st_size
            if len(files) + 1 > MAX_FIXTURE_FILES or total_bytes > MAX_FIXTURE_BYTES:
                raise ValueError("task fixture exceeds the file or byte limit")
            try:
                content = path.read_text(encoding="utf-8")
            except UnicodeDecodeError as exc:
                raise ValueError(
                    f"fixture file must be UTF-8 text: {relative}"
                ) from exc
            files.append((relative, content))
        if not files:
            raise ValueError("task fixture project is empty")
        fixture_paths = {relative for relative, _content in files}
        missing = set(self.allowed_paths).difference(fixture_paths)
        if missing:
            raise ValueError(
                "allowed paths must already exist in the fixture: "
                + ", ".join(sorted(missing))
            )
        return tuple(files)

    def parent_prompt(self, continuity_token: str) -> str:
        return (
            f"Plan this task without editing files:\n\n{self.planning_instructions}\n\n"
            f"Remember the continuity token {continuity_token} for child sessions. "
            "Do not write the token anywhere. Respond only with a concise "
            "implementation plan."
        )

    def candidate_prompt(
        self,
        *,
        candidate_name: str,
        strategy: str,
        workspace: str,
        report_path: str,
    ) -> str:
        editable = ", ".join(self.allowed_paths)
        return (
            f"Continue the parent plan using this strategy: {strategy}\n\n"
            f"Task:\n{self.implementation_instructions}\n\n"
            f"Work in {workspace}. You may modify only: {editable}. "
            f"Run {self.test_command!r}. Then write {report_path} with exactly "
            f"two lines: CANDIDATE={candidate_name} and TOKEN=<the continuity "
            "token from the parent conversation>. Also include "
            "CONTINUITY=<that token> in your final response. Do not read the "
            "token from files."
        )


def load_rollout_task(path: str | Path) -> RolloutTask:
    """Load and validate a task.json next to its project/ fixture."""
    config_path = Path(path).expanduser().resolve()
    payload = json.loads(config_path.read_text(encoding="utf-8"))
    if not isinstance(payload, dict):
        raise ValueError("task configuration must be a JSON object")

    raw_paths = payload.get("allowed_paths")
    if not isinstance(raw_paths, list) or not raw_paths:
        raise ValueError("task field 'allowed_paths' must be a non-empty list")
    allowed_paths = tuple(
        _safe_relative_path(item)
        for item in raw_paths
        if isinstance(item, str)
    )
    if len(allowed_paths) != len(raw_paths) or len(set(allowed_paths)) != len(
        allowed_paths
    ):
        raise ValueError("task allowed_paths must contain unique strings")

    raw_strategies = payload.get("strategies")
    if not isinstance(raw_strategies, list) or not raw_strategies:
        raise ValueError("task field 'strategies' must be a non-empty list")
    strategies: list[tuple[str, str]] = []
    for item in raw_strategies:
        if not isinstance(item, dict):
            raise ValueError("each task strategy must be an object")
        name = _required_string(item, "name")
        instructions = _required_string(item, "instructions")
        if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", name):
            raise ValueError(f"task strategy has an unsafe name: {name!r}")
        strategies.append((name, instructions))
    if len({name for name, _instructions in strategies}) != len(strategies):
        raise ValueError("task strategy names must be unique")

    baseline = payload.get("expect_baseline_failure", True)
    if not isinstance(baseline, bool):
        raise ValueError("task field 'expect_baseline_failure' must be boolean")

    task = RolloutTask(
        name=_required_string(payload, "name"),
        summary=_required_string(payload, "summary"),
        planning_instructions=_required_string(payload, "planning_instructions"),
        implementation_instructions=_required_string(
            payload, "implementation_instructions"
        ),
        test_command=_required_string(payload, "test_command"),
        test_timeout_seconds=_positive_int(payload, "test_timeout_seconds"),
        allowed_paths=allowed_paths,
        strategies=tuple(strategies),
        expect_baseline_failure=baseline,
        project_dir=config_path.parent / "project",
    )
    task.fixture_files()
    return task
