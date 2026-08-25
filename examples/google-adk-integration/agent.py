"""Google ADK agent wired to CubeSandbox for code execution."""

from __future__ import annotations

import os

from dotenv import load_dotenv
from google.adk import Agent

from cube_code_tool import run_python_in_cube


load_dotenv(override=False)

root_agent = Agent(
    name="cube_code_agent",
    model=os.environ.get("GOOGLE_ADK_MODEL", "gemini-2.5-flash"),
    instruction=(
        "You are a careful coding assistant. When the user asks you to run, "
        "test, or inspect Python code, call run_python_in_cube so execution "
        "happens inside an isolated CubeSandbox MicroVM. Explain results from "
        "the tool output instead of claiming execution happened locally."
    ),
    tools=[run_python_in_cube],
)
