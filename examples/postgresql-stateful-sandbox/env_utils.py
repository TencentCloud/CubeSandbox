# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Environment helpers shared by the PostgreSQL examples."""

import os
from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Load the nearest example ``.env`` without overriding real variables."""
    candidate_paths = [
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ]

    for path in candidate_paths:
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def require_env(name: str) -> str:
    """Return a required environment variable or raise a useful error."""
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(
            f"{name} is required. Copy .env.example to .env and set it first."
        )
    return value
