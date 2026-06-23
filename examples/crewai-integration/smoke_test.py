# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Verify CrewAI's E2B tool can execute Python through Cube Sandbox."""

import os

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


def main() -> None:
    """Run one deterministic Python cell without invoking an LLM."""
    cube_python = E2BPythonTool(
        template=require_environment(),
        persistent=False,
        sandbox_timeout=120,
    )
    result = cube_python.run(
        code=(
            "import json\n"
            "payload = {'runtime': 'cube', 'sum': sum(range(10))}\n"
            "print(json.dumps(payload, sort_keys=True))"
        ),
        timeout=30,
    )
    result_text = str(result)
    if not all(fragment in result_text for fragment in ("runtime", "cube", "45")):
        raise RuntimeError(f"Unexpected Cube smoke test result: {result_text}")
    print(result)


if __name__ == "__main__":
    main()
