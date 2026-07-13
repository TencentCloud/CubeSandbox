"""Shared configuration and readiness helpers for the Ruby template demos."""

from __future__ import annotations

import os
import time
from pathlib import Path

import requests
from dotenv import load_dotenv


def load_environment() -> None:
    """Load the example's local environment file explicitly."""
    load_dotenv(Path(__file__).with_name(".env"))


def required(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def public_url(sandbox, path: str = "/health") -> str:
    return f"https://{sandbox.get_host(4567)}{path}"


def tls_verify() -> str | bool:
    """Return a CA bundle path when configured, otherwise verify normally."""
    return os.getenv("REQUESTS_CA_BUNDLE") or True


def wait_for_app(sandbox, timeout: int = 60) -> dict:
    url = public_url(sandbox)
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            response = requests.get(url, timeout=5, verify=tls_verify())
            response.raise_for_status()
            health = response.json()
            if health.get("status") != "ok" or not health.get("ruby"):
                raise RuntimeError(f"Unexpected health response: {health!r}")
            return health
        except (requests.RequestException, ValueError) as exc:
            last_error = exc
            time.sleep(1)
    raise RuntimeError(f"Sinatra did not become ready at {url}: {last_error}")
