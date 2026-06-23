# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a CrewAI data-analysis task inside Cube Sandbox."""

import os
from typing import Any

from crewai import Agent, Crew, LLM, Process, Task
from crewai_tools import E2BPythonTool
from dotenv import load_dotenv


def require_environment() -> None:
    """Load local configuration and fail early when required values are absent."""
    load_dotenv()
    required = ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID", "OPENAI_API_KEY")
    missing = [name for name in required if not os.getenv(name)]
    if missing:
        raise RuntimeError(f"Missing required environment variables: {', '.join(missing)}")


def create_llm() -> LLM:
    """Create a CrewAI LLM from OpenAI or an OpenAI-compatible endpoint."""
    options: dict[str, Any] = {
        "model": os.getenv("MODEL", "openai/gpt-4o-mini"),
        "api_key": os.environ["OPENAI_API_KEY"],
    }
    if base_url := os.getenv("OPENAI_BASE_URL"):
        options["base_url"] = base_url
    return LLM(**options)


def main() -> None:
    """Create a sandboxed analyst and execute a deterministic simulation task."""
    require_environment()

    cube_python = E2BPythonTool(
        template=os.environ["CUBE_TEMPLATE_ID"],
        persistent=False,
        sandbox_timeout=120,
    )

    analyst = Agent(
        role="Sandboxed data analyst",
        goal="Use isolated Python execution to produce reproducible answers",
        backstory=(
            "You verify every numerical result by running Python in Cube Sandbox. "
            "Never execute generated code on the host."
        ),
        tools=[cube_python],
        llm=create_llm(),
        verbose=os.getenv("CREWAI_VERBOSE", "").lower() == "true",
    )

    task = Task(
        description=(
            "You must use the E2B Sandbox Python tool. Set random seed 7, simulate "
            "10,000 rolls of two fair six-sided dice, and estimate the probability "
            "that their sum is 8. Compare the estimate with the exact probability "
            "5/36. Include the Python method and all numerical results."
        ),
        expected_output=(
            "A short report containing the simulated probability, exact probability, "
            "absolute error, and the Python method used."
        ),
        agent=analyst,
    )

    result = Crew(
        agents=[analyst],
        tasks=[task],
        process=Process.sequential,
    ).kickoff()
    print(result)


if __name__ == "__main__":
    main()
