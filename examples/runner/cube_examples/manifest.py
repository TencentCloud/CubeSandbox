"""Manifest parsing and validation for ``cube-example.yaml`` files.

A manifest declares how to set up, run, and assert one example. The schema is
intentionally small and declarative:

.. code-block:: yaml

    name: code-sandbox-quickstart
    description: Create a sandbox and run code/commands inside it.
    lang: python
    tags: [smoke, sdk]
    timeout: 180
    requires_template: code        # logical template alias (see TEMPLATE_ALIASES)
    setup:
      - pip install -r requirements.txt
    env:
      E2B_API_KEY: e2b_000000
    steps:
      - name: create
        run: python create.py
        expect_exit: 0
        expect_stdout_contains: ["sandbox info"]
        expect_stdout_not_contains: ["Traceback"]
      - name: exec_code
        run: python exec_code.py
        expect_stdout_contains: ["hello cube"]
"""

from __future__ import annotations

import dataclasses
from pathlib import Path
from typing import Any

try:
    import yaml
except ImportError as exc:  # pragma: no cover - dependency guard
    raise SystemExit(
        "PyYAML is required. Install with: pip install pyyaml"
    ) from exc


MANIFEST_FILENAME = "cube-example.yaml"


class ManifestError(ValueError):
    """Raised when a manifest is malformed."""


@dataclasses.dataclass
class Step:
    """A single command to run within an example."""

    name: str
    run: str
    expect_exit: int | None = 0
    expect_stdout_contains: list[str] = dataclasses.field(default_factory=list)
    expect_stdout_not_contains: list[str] = dataclasses.field(default_factory=list)
    timeout: int | None = None
    allow_failure: bool = False

    @classmethod
    def from_dict(cls, data: dict[str, Any], idx: int) -> "Step":
        if "run" not in data:
            raise ManifestError(f"step #{idx} missing required field 'run'")
        name = str(data.get("name") or f"step-{idx}")
        return cls(
            name=name,
            run=str(data["run"]),
            expect_exit=(
                None if data.get("expect_exit", 0) is None else int(data.get("expect_exit", 0))
            ),
            expect_stdout_contains=_as_str_list(data.get("expect_stdout_contains")),
            expect_stdout_not_contains=_as_str_list(data.get("expect_stdout_not_contains")),
            timeout=(int(data["timeout"]) if data.get("timeout") is not None else None),
            allow_failure=bool(data.get("allow_failure", False)),
        )


@dataclasses.dataclass
class Manifest:
    """A parsed ``cube-example.yaml``."""

    name: str
    path: Path  # example directory
    description: str = ""
    lang: str = "python"
    tags: list[str] = dataclasses.field(default_factory=list)
    timeout: int = 300
    requires_template: str | None = None
    setup: list[str] = dataclasses.field(default_factory=list)
    env: dict[str, str] = dataclasses.field(default_factory=dict)
    steps: list[Step] = dataclasses.field(default_factory=list)
    skip: bool = False
    skip_reason: str = ""

    @classmethod
    def load(cls, manifest_path: Path) -> "Manifest":
        try:
            raw = yaml.safe_load(manifest_path.read_text(encoding="utf-8")) or {}
        except yaml.YAMLError as exc:
            raise ManifestError(f"{manifest_path}: invalid YAML: {exc}") from exc
        if not isinstance(raw, dict):
            raise ManifestError(f"{manifest_path}: top level must be a mapping")

        example_dir = manifest_path.parent
        name = str(raw.get("name") or example_dir.name)
        steps_raw = raw.get("steps") or []
        if not isinstance(steps_raw, list):
            raise ManifestError(f"{manifest_path}: 'steps' must be a list")
        steps = [Step.from_dict(s, i) for i, s in enumerate(steps_raw) if isinstance(s, dict)]

        return cls(
            name=name,
            path=example_dir,
            description=str(raw.get("description", "")),
            lang=str(raw.get("lang", "python")),
            tags=_as_str_list(raw.get("tags")),
            timeout=int(raw.get("timeout", 300)),
            requires_template=(
                str(raw["requires_template"]) if raw.get("requires_template") else None
            ),
            setup=_as_str_list(raw.get("setup")),
            env={str(k): str(v) for k, v in (raw.get("env") or {}).items()},
            steps=steps,
            skip=bool(raw.get("skip", False)),
            skip_reason=str(raw.get("skip_reason", "")),
        )


def _as_str_list(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, str):
        return [value]
    if isinstance(value, (list, tuple)):
        return [str(v) for v in value]
    raise ManifestError(f"expected string or list, got {type(value).__name__}")


def discover(examples_root: Path) -> list[Manifest]:
    """Find and parse all manifests under ``examples_root``."""
    manifests: list[Manifest] = []
    for manifest_path in sorted(examples_root.rglob(MANIFEST_FILENAME)):
        manifests.append(Manifest.load(manifest_path))
    return manifests
