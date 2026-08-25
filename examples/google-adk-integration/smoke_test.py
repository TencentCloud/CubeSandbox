"""Offline smoke checks for the Google ADK integration example."""

from __future__ import annotations

import ast
import inspect

import agent
import cube_code_tool


def main() -> None:
    assert agent.root_agent.name == "cube_code_agent"
    assert cube_code_tool.run_python_in_cube in agent.root_agent.tools

    signature = inspect.signature(cube_code_tool.run_python_in_cube)
    assert list(signature.parameters) == ["code"]

    ast.parse(
        """
numbers = [1, 2, 3, 4]
print(sum(numbers))
"""
    )
    print("GOOGLE_ADK_CUBE_SMOKE_OK")


if __name__ == "__main__":
    main()
