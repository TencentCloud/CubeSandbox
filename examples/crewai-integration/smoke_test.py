# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Verify CrewAI's E2B tool can execute Python through Cube Sandbox."""

import json
import os
from collections.abc import Iterable
from typing import Any

from crewai_tools import E2BPythonTool
from dotenv import load_dotenv


def require_environment() -> str:
    """Load local configuration and return the Cube template ID."""
    load_dotenv()
    required = ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID")
    missing = [name for name in required if not os.getenv(name)]
    if missing:
        raise RuntimeError(f"Missing required environment variables: {', '.join(missing)}")
    return os.environ["CUBE_TEMPLATE_ID"]


def stdout_from_execution(result: Any) -> str:
    """Extract stdout from E2B execution results without falling back to errors."""
    logs = getattr(result, "logs", None)
    stdout = getattr(logs, "stdout", None) if logs is not None else None
    if stdout is None:
        stdout = getattr(result, "stdout", None)

    if isinstance(stdout, str):
        return stdout.strip()
    if isinstance(stdout, Iterable):
        return "\n".join(str(item) for item in stdout).strip()
    return str(result).strip()


def validate_smoke_result(result: Any) -> dict[str, Any]:
    """Parse and validate the exact JSON payload emitted by the sandbox."""
    error = getattr(result, "error", None)
    if error:
        raise RuntimeError(f"Cube smoke test returned an execution error: {error}")

    stdout = stdout_from_execution(result)
    for line in reversed(stdout.splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        if (
            isinstance(payload, dict)
            and payload.get("runtime") == "cube"
            and payload.get("sum") == 45
        ):
            return payload
        raise RuntimeError(f"Unexpected Cube smoke test payload: {payload!r}")

    raise RuntimeError(f"Cube smoke test did not emit a JSON payload: {stdout!r}")


def main() -> None:
    """Run one deterministic Python cell without invoking an LLM."""
    cube_python = E2BPythonTool(
        template=require_environment(),
        persistent=False,
    )
    result = cube_python.run(
        code=(
            "import json\n"
            "payload = {'runtime': 'cube', 'sum': sum(range(10))}\n"
            "print(json.dumps(payload, sort_keys=True))"
        ),
        timeout=30,
    )
    payload = validate_smoke_result(result)
    print(json.dumps(payload, sort_keys=True))


if __name__ == "__main__":
    main()
