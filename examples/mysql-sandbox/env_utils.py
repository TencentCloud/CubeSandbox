"""Shared environment utilities for Cube Sandbox examples.

This module centralizes all environment variable access for the mysql-sandbox
examples. It provides:

- Fallback defaults for optional variables (API URL, SSL cert)
- Strict validation for required variables (API key, template ID)
- Shared helpers for building and executing MySQL commands safely

Security design:
- Passwords are never passed on the command line; they are injected as
  sandbox environment variables (MYSQL_PWD) so they never appear in
  /proc/<pid>/cmdline or `ps aux` output.
- Database identifiers (host, user, database name) are shell-quoted via
  shlex.quote() to prevent command injection through environment variables.
- SQL statements are passed via heredoc (<<'SQL') to prevent shell expansion.
"""

import os
import shlex
from typing import TYPE_CHECKING, Optional

if TYPE_CHECKING:
    from e2b_code_interpreter import Sandbox

# Lazy-load dotenv: optional dependency, scripts work without it if
# environment variables are exported before running.
try:
    from dotenv import load_dotenv

    _has_dotenv = True
except ImportError:
    _has_dotenv = False
    load_dotenv = None

# Load .env once at import time (not on every get_env() call).
# override=False preserves any variables already set in the environment,
# allowing command-line exports to take precedence over .env file values.
if _has_dotenv:
    load_dotenv(override=False)


def get_env(key: str, default: Optional[str] = None) -> Optional[str]:
    """Get environment variable with optional default.

    .env files are loaded once at module import time via ``load_dotenv()``.
    Subsequent calls only read from ``os.environ``.

    Args:
        key: Environment variable name.
        default: Value returned if the variable is not set.

    Returns:
        The environment variable value, or ``default`` if not set.
    """
    return os.environ.get(key, default)


def require_env(key: str) -> str:
    """Get a required environment variable, raising ValueError if not set."""
    value = get_env(key)
    if not value:
        raise ValueError(
            f"Required environment variable '{key}' is not set. "
            f"Please set it in .env file or export it."
        )
    return value


def get_template_id() -> str:
    """Get Cube template ID from environment (required)."""
    return require_env("CUBE_TEMPLATE_ID")


def get_api_url() -> str:
    """Get Cube API URL from environment with default for local development."""
    return get_env("E2B_API_URL", "http://127.0.0.1:3000")


def get_api_key() -> str:
    """Get Cube API key from environment (required for SDK authentication).

    Raises:
        ValueError: If E2B_API_KEY is not set.
    """
    key = get_env("E2B_API_KEY")
    if not key:
        raise ValueError(
            "E2B_API_KEY is not set. Please set it in .env file or export it. "
            "The API key can be any non-empty value for local development."
        )
    return key


def get_ssl_cert_file() -> Optional[str]:
    """Get SSL certificate file path from environment (optional).

    Set this when using HTTPS with a custom CA certificate (e.g., mkcert).
    Most systems work without setting this as the default CA bundle is used.
    """
    return get_env("SSL_CERT_FILE")


def build_mysql_cmd(database: str = "") -> str:
    """Build a `mysql` shell command safe for execution inside a sandbox.

    Two hardening layers are applied:

    1. **No password on the command line.**
       The MySQL client natively reads ``MYSQL_PWD`` from the environment,
       so we inject the password via the sandbox ``envs=`` parameter and
       deliberately omit ``-p<password>``. This keeps the password out of
       ``/proc/<pid>/cmdline`` and ``ps aux`` inside the sandbox.

    2. **Shell-quoted connection args.**
       ``DB_HOST`` / ``DB_USER`` / ``database`` are passed through
       :func:`shlex.quote` so that a misconfiguration like
       ``DB_NAME="test_db --ssl-mode=DISABLED"`` or
       ``DB_NAME="test_db; touch /tmp/pwned"`` is treated as a single
       argument (or rejected as a malformed database name by the server)
       rather than being expanded by the shell.

    The SQL statement itself is passed separately via a quoted heredoc
    (``<<'SQL'``) in :func:`run_mysql_query` to avoid ``-e '...'`` quoting hazards.

    Args:
        database: Optional database name to connect to (shell-quoted automatically).

    Returns:
        A shell command string ready to be passed to ``Sandbox.commands.run()``.
    """
    db_host = os.environ.get("DB_HOST", "localhost")
    db_user = os.environ.get("DB_USER", "root")
    parts = ["mysql", "-h", shlex.quote(db_host), "-u", shlex.quote(db_user)]
    if database:
        parts.append(shlex.quote(database))
    # --table gives consistent tabular output regardless of the user's
    # local my.cnf, which makes scripted parsing less surprising.
    parts.append("--table")
    return " ".join(parts)


def run_mysql_query(
    sandbox: "Sandbox",
    query: str,
    database: str = "",
    check: bool = True,
) -> str:
    """Execute a SQL statement safely inside a sandbox and return stdout.

    Three hardening layers protect against credential leakage and command
    injection:

    1. **Password via MYSQL_PWD** — the password is passed as a sandbox
       environment variable, never on the command line.
    2. **Shell-quoted identifiers** — ``build_mysql_cmd`` wraps host, user,
       and database name through :func:`shlex.quote`.
    3. **Heredoc for SQL body** — the query text is fed through a
       single-quoted heredoc (``<<'SQL'``), which prevents the shell from
       expanding variables or interpreting metacharacters in the SQL.

    Args:
        sandbox: Active :class:`e2b_code_interpreter.Sandbox` instance.
        query: SQL statement to execute.
        database: Optional database name to connect to.
        check: If True (default), print a warning on non-zero exit codes.

    Returns:
        The stdout from the mysql client (may be empty string on success
        or when the query returns no rows).
    """
    base = build_mysql_cmd(database=database)
    # Single-quoted heredoc prevents shell expansion of $variables or
    # backtick commands inside the SQL statement.
    cmd = f"{base} <<'SQL'\n{query}\nSQL"
    result = sandbox.commands.run(cmd)
    if check and result.exit_code != 0:
        print(f"    Warning: command exited with code {result.exit_code}")
        if result.stderr:
            print(f"    Error: {result.stderr.strip()}")
    elif result.stderr:
        # MySQL sometimes outputs informational messages to stderr even on success
        # (e.g., "Reading table information..."). These are warnings, not errors.
        print(f"    Warning: {result.stderr.strip()}")
    return result.stdout or ""
