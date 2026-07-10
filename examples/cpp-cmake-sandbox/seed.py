# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Shared helper that pushes the local `project/` tree into a sandbox.
#
# Works with both the E2B-compatible SDK (e2b_code_interpreter.Sandbox) and the
# native cubesandbox SDK, since both expose the same `sandbox.files.write(path,
# data)` method.

from pathlib import Path
from typing import Iterator, Tuple

PROJECT_DIR = Path(__file__).with_name("project")

#: Destination directory for the project inside the sandbox.
SANDBOX_PROJECT_DIR = "/workspace/project"


def iter_project_files() -> Iterator[Tuple[str, str]]:
    """Yield ``(relative_posix_path, text_content)`` for every project file."""
    for path in sorted(PROJECT_DIR.rglob("*")):
        if path.is_file():
            rel = path.relative_to(PROJECT_DIR).as_posix()
            yield rel, path.read_text(encoding="utf-8")


def push_project(sandbox, dest: str = SANDBOX_PROJECT_DIR) -> int:
    """Copy the whole project into ``dest`` inside the sandbox.

    Returns the number of files written.
    """
    count = 0
    for rel, content in iter_project_files():
        sandbox.files.write(f"{dest}/{rel}", content)
        count += 1
    return count
