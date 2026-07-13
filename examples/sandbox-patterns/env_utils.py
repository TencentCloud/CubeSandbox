# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
from pathlib import Path

from dotenv import load_dotenv


class MissingEnvironmentVariable(Exception):
    """Raised when a required environment variable is not set."""


def load_local_dotenv() -> None:
    """Load a .env file from the script's directory or CWD.

    Uses path deduplication via ``seen_paths`` and resolves symlinks so
    the same physical file is not loaded twice. Existing environment
    variables always take precedence (override=False).
    """
    candidate_paths = [
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ]

    seen_paths = set()
    for path in candidate_paths:
        resolved_path = path.resolve()
        if resolved_path in seen_paths:
            continue
        seen_paths.add(resolved_path)

        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise MissingEnvironmentVariable(name)
    return value
