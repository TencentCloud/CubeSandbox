#!/usr/bin/env python3
"""
Environment utilities for Cube Sandbox MySQL examples.

Loads environment variables from .env file and provides helper functions
for sandbox operations.
"""

import os
from pathlib import Path
from typing import Optional

from dotenv import load_dotenv

# ----------------------------------------------------------------------
# .env loading
# ----------------------------------------------------------------------


def _load_env() -> None:
    """Load .env file from script directory or current directory."""
    # Try script's directory first
    script_dir = Path(__file__).parent
    env_file = script_dir / ".env"
    if env_file.exists():
        load_dotenv(env_file)
        return

    # Try current directory
    cwd_env = Path.cwd() / ".env"
    if cwd_env.exists():
        load_dotenv(cwd_env)
        return

    # Try parent directories up to root
    for path in Path.cwd().parents:
        env_file = path / ".env"
        if env_file.exists():
            load_dotenv(env_file)
            return


_load_env()


# ----------------------------------------------------------------------
# Helper functions
# ----------------------------------------------------------------------


def get_api_url(default: Optional[str] = None) -> str:
    """Get E2B_API_URL from environment."""
    value = os.getenv("E2B_API_URL", default)
    if not value:
        raise ValueError(
            "E2B_API_URL is not set. "
            "Please set it in .env file or export it to your environment. "
            "Example: export E2B_API_URL=http://127.0.0.1:3000"
        )
    return value


def get_api_key(default: Optional[str] = None) -> str:
    """Get E2B_API_KEY from environment."""
    value = os.getenv("E2B_API_KEY", default)
    if not value:
        raise ValueError(
            "E2B_API_KEY is not set. "
            "Please set it in .env file or export it to your environment. "
            "Example: export E2B_API_KEY=e2b_000000"
        )
    return value


def get_template_id() -> str:
    """Get CUBE_TEMPLATE_ID from environment."""
    value = os.getenv("CUBE_TEMPLATE_ID")
    if not value:
        raise ValueError(
            "CUBE_TEMPLATE_ID is not set. "
            "Please set it in .env file or export it to your environment. "
            "Example: export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxx"
        )
    return value


def get_ssl_cert_file() -> Optional[str]:
    """Get SSL_CERT_FILE from environment (optional)."""
    return os.getenv("SSL_CERT_FILE")


def get_env(key: str, default: Optional[str] = None) -> Optional[str]:
    """Get arbitrary environment variable with optional default."""
    return os.getenv(key, default)


def load_env(key: str, required: bool = True, default: str = None) -> str:
    """
    Load environment variable from .env file or system environment.

    Args:
        key: Environment variable name
        required: If True, raise error when key is not found
        default: Default value if key is not found and required=False

    Returns:
        The environment variable value

    Raises:
        ValueError: If required=True and key is not found
    """
    value = os.getenv(key, default)

    if required and value is None:
        raise ValueError(
            f"Environment variable '{key}' is not set. "
            f"Please set it in .env file or export it to your environment. "
            f"See .env.example for reference."
        )

    return value


def run_mysql_query(sandbox, query: str, database: str = "") -> str:
    """
    Execute a MySQL query in the sandbox and return the output.

    Args:
        sandbox: The Cube Sandbox instance
        query: SQL query to execute
        database: Optional database name to USE before running query

    Returns:
        Query output as string, or empty string if no output
    """
    # Build the mysql command with proper escaping
    cmd_parts = ["mysql", "-s", "-N"]

    if database:
        cmd_parts.extend(["--database", database])

    # Execute the query
    if query.strip():
        result = sandbox.commands.run(
            f"{' '.join(cmd_parts)} -e {query!r}",
            timeout=30,
        )
    else:
        # No query, just connect (for commands that need USE first)
        result = sandbox.commands.run(" ".join(cmd_parts), timeout=30)

    if result.exit_code != 0:
        if result.stderr:
            print(f"    Warning: {result.stderr.strip()}")
        return ""

    return result.stdout


# ----------------------------------------------------------------------
# Legacy compatibility
# ----------------------------------------------------------------------


def setup_environment() -> None:
    """Setup environment variables from .env file."""
    _load_env()


if __name__ == "__main__":
    # Quick test
    print("Testing env_utils...")
    try:
        print(f"API URL: {get_api_url()}")
        print(f"Template ID: {get_template_id()}")
    except ValueError as e:
        print(f"Expected error (not configured): {e}")
