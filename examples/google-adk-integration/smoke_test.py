"""Offline smoke checks for the Google ADK integration example."""

from __future__ import annotations

import inspect
from typing import Any

import agent
import cube_code_tool


def _tool_names(tools: list[Any]) -> list[str]:
    names: list[str] = []
    for tool in tools:
        name = getattr(tool, "name", None)
        if name:
            names.append(str(name))
            continue

        func = getattr(tool, "func", None)
        func_name = getattr(func, "__name__", None)
        if func_name:
            names.append(str(func_name))
            continue

        direct_name = getattr(tool, "__name__", None)
        if direct_name:
            names.append(str(direct_name))

    return names


def main() -> None:
    assert agent.root_agent.name == "cube_code_agent"
    assert "run_python_in_cube" in _tool_names(agent.root_agent.tools)

    signature = inspect.signature(cube_code_tool.run_python_in_cube)
    assert list(signature.parameters) == ["code"]

    class LogItem:
        line = "hello\n"

    class Logs:
        stdout = [LogItem(), "world\n"]

    class Execution:
        logs = Logs()
        text = "hello world"
        error = None
        results = []

    result = cube_code_tool._execution_to_dict(Execution(), [])
    assert result["stdout"] == "hello\nworld\n"
    assert result["text"] == "hello world"
    assert result["error"] is None

    class EmptyLogs:
        stdout: list[str] = []

    class EmptyLogExecution:
        logs = EmptyLogs()
        text = ""
        error = None

    fallback = cube_code_tool._execution_to_dict(EmptyLogExecution(), ["callback\n"])
    assert fallback["stdout"] == "callback\n"
    print("GOOGLE_ADK_CUBE_SMOKE_OK")


if __name__ == "__main__":
    main()
