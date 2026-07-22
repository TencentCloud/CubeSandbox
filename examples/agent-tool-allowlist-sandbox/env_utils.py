# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    # Use resolved paths for existence + load. (Sibling examples may still use
    # unresolved path after resolve-only dedup; early return makes dedup a no-op.)
    for path in (
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ):
        resolved_path = path.resolve()
        if resolved_path.is_file():
            load_dotenv(dotenv_path=resolved_path, override=False)
            return
