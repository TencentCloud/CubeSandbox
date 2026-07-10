"""Small environment helpers shared by the host-side example scripts."""

from __future__ import annotations

import os
from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Load a nearby .env without replacing explicitly exported variables."""
    for candidate in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if candidate.is_file():
            load_dotenv(candidate, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value
