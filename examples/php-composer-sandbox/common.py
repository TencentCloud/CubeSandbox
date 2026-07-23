"""Small, dependency-light helpers shared by the runnable PHP sandbox demos."""

from __future__ import annotations

import os
import time
from pathlib import Path
from typing import Any, Callable


def load_local_dotenv() -> None:
    """Load a nearby .env file without replacing exported environment variables."""
    from dotenv import load_dotenv

    for candidate in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if candidate.is_file():
            load_dotenv(candidate, override=False)
            return


def template_id() -> str:
    value = os.environ.get("CUBE_TEMPLATE_ID", "").strip()
    if not value or value == "tpl_replace_me":
        raise RuntimeError("CUBE_TEMPLATE_ID is required; copy .env.example to .env and update it")
    return value


def application_url(sandbox: Any, port: int = 8080) -> str:
    """Return the CubeProxy HTTP URL for an application port."""
    return f"http://{sandbox.get_host(port)}"


def wait_for_health(
    get: Callable[..., Any],
    url: str,
    *,
    attempts: int = 20,
    delay_seconds: float = 1.0,
) -> dict[str, Any]:
    """Wait for the PHP application instead of assuming it is ready after boot."""
    last_error: Exception | None = None
    for _ in range(attempts):
        try:
            response = get(f"{url}/health", timeout=5)
            if response.status_code == 200:
                payload = response.json()
                if payload.get("status") == "ok" and payload.get("runtime") == "php":
                    return payload
                last_error = RuntimeError(f"unexpected health payload: {payload!r}")
            else:
                last_error = RuntimeError(f"health endpoint returned HTTP {response.status_code}")
        except Exception as exc:  # requests can raise several transport-specific errors.
            last_error = exc
        time.sleep(delay_seconds)
    raise RuntimeError(f"PHP application did not become healthy at {url}") from last_error
