"""CubeSandbox-backed code execution tool for Google ADK."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox


def _load_env() -> None:
    """Load local .env values without overriding exported environment values."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            load_dotenv(path, override=False)
            break

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
    if cube_ssl and Path(cube_ssl).is_file():
        os.environ.setdefault("SSL_CERT_FILE", cube_ssl)


def _read_required_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"Missing required environment variable: {name}")
    return value


def _stringify_log_item(item: Any) -> str:
    line = getattr(item, "line", None)
    if line is not None:
        return str(line)
    return str(item)


def _execution_to_dict(execution: Any, stdout: list[Any]) -> dict[str, Any]:
    """Return a JSON-serializable result across E2B SDK versions."""
    logs = getattr(execution, "logs", None)
    captured_stdout = getattr(logs, "stdout", None) if logs else None
    stdout_items = captured_stdout if captured_stdout is not None else stdout

    result: dict[str, Any] = {
        "stdout": "".join(_stringify_log_item(item) for item in stdout_items),
        "text": getattr(execution, "text", None),
        "error": None,
    }

    error = getattr(execution, "error", None)
    if error:
        result["error"] = str(error)

    results = getattr(execution, "results", None)
    if results is not None:
        result["results_count"] = len(results)

    return result


def run_python_in_cube(code: str) -> dict[str, Any]:
    """Execute Python code inside a temporary CubeSandbox MicroVM.

    Args:
        code: Python source code to execute in the sandbox.

    Returns:
        A dictionary containing stdout, text output, and any execution error.
    """
    _load_env()

    if not code or not code.strip():
        return {"stdout": "", "text": "", "error": "code must not be empty"}

    template_id = _read_required_env("CUBE_TEMPLATE_ID")
    timeout = int(os.environ.get("CUBE_SANDBOX_TIMEOUT", "300"))

    stdout: list[Any] = []
    with Sandbox.create(template=template_id, timeout=timeout) as sandbox:
        execution = sandbox.run_code(code, on_stdout=stdout.append)
        return _execution_to_dict(execution, stdout)
