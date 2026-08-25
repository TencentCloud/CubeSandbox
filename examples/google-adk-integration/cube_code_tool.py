"""CubeSandbox-backed code execution tool for Google ADK."""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Any

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox


def load_environment() -> None:
    """Load local .env values without overriding exported environment values."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            load_dotenv(path, override=False)
            break

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
    if cube_ssl and Path(cube_ssl).is_file():
        os.environ.setdefault("SSL_CERT_FILE", cube_ssl)


def _read_int_env(name: str, default: int) -> int:
    value = os.environ.get(name)
    if value is None or not value.strip():
        return default
    try:
        return int(value)
    except ValueError as exc:
        raise RuntimeError(f"{name} must be an integer number of seconds") from exc


def _bool_env(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def _setup_optional_dev_sidecar() -> None:
    if not _bool_env("CUBE_USE_DEV_SIDECAR"):
        return

    sidecar_dir = Path(__file__).resolve().parents[1] / "e2b-dev-sidecar"
    sys.path.insert(0, str(sidecar_dir))
    try:
        from dev_sidecar import setup_dev_sidecar
    except ImportError as exc:
        raise RuntimeError(
            "CUBE_USE_DEV_SIDECAR is enabled, but examples/e2b-dev-sidecar "
            "could not be imported. Install this example's requirements and "
            "run it from the CubeSandbox repository checkout."
        ) from exc

    setup_dev_sidecar()


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
    stdout_items = captured_stdout or stdout

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
    load_environment()

    if not code or not code.strip():
        return {"stdout": "", "text": "", "error": "code must not be empty"}

    template_id = _read_required_env("CUBE_TEMPLATE_ID")
    sandbox_timeout = _read_int_env("CUBE_SANDBOX_TIMEOUT", 300)
    run_code_timeout = _read_int_env("CUBE_RUN_CODE_TIMEOUT", 60)

    stdout: list[Any] = []
    _setup_optional_dev_sidecar()
    with Sandbox.create(template=template_id, timeout=sandbox_timeout) as sandbox:
        execution = sandbox.run_code(
            code,
            on_stdout=stdout.append,
            timeout=run_code_timeout,
        )
        return _execution_to_dict(execution, stdout)
