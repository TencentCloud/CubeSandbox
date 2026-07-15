"""Shared environment utilities for Cube Sandbox examples."""
import os
from pathlib import Path
from typing import Optional

try:
    from dotenv import load_dotenv
    _has_dotenv = True
except ImportError:
    _has_dotenv = False


def get_env(key: str, default: Optional[str] = None) -> Optional[str]:
    """Get environment variable with optional .env file support."""
    if _has_dotenv:
        # Try to load from .env in current directory or script directory
        load_dotenv(override=False)
    return os.environ.get(key, default)


def require_env(key: str) -> str:
    """Get required environment variable, raise if not set."""
    value = get_env(key)
    if not value:
        raise ValueError(
            f"Required environment variable '{key}' is not set. "
            f"Please set it in .env file or export it."
        )
    return value


def get_template_id() -> str:
    """Get Cube template ID from environment."""
    return require_env("CUBE_TEMPLATE_ID")


def get_api_url() -> str:
    """Get Cube API URL from environment."""
    return get_env("E2B_API_URL", "http://127.0.0.1:3000")


def get_api_key() -> str:
    """Get E2B API key from environment."""
    return get_env("E2B_API_KEY", "e2b_000000")


def get_ssl_cert_file() -> Optional[str]:
    """Get SSL certificate file path from environment."""
    return get_env("SSL_CERT_FILE")
