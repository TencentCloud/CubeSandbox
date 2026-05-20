# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .sandbox import Sandbox


@dataclass
class CommandResult:
    stdout: str
    stderr: str
    exit_code: int


class Commands:
    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    def run(self, cmd: str, *, timeout: float | None = None, **kwargs) -> CommandResult:
        """Run a shell command inside the sandbox via Python subprocess."""
        code = (
            "import subprocess as _sp\n"
            f"_r = _sp.run({cmd!r}, shell=True, capture_output=True, text=True)\n"
            "import sys as _sys\n"
            "_sys.stdout.write(_r.stdout)\n"
            "_sys.stderr.write(_r.stderr)\n"
            "print(_r.returncode)\n"
        )
        stdout_lines: list[str] = []
        execution = self._sandbox.run_code(
            code,
            timeout=timeout,
            on_stdout=lambda m: stdout_lines.append(m.text),
        )
        # last stdout line is the exit code printed by the wrapper
        all_stdout = "".join(stdout_lines)
        lines = all_stdout.splitlines()
        if lines and lines[-1].strip().lstrip("-").isdigit():
            exit_code = int(lines[-1].strip())
            stdout = "\n".join(lines[:-1])
            if lines[:-1]:  # restore trailing newline if there was content
                stdout += "\n"
        else:
            exit_code = 1 if execution.error else 0
            stdout = all_stdout
        stderr = "".join(execution.logs.stderr)
        return CommandResult(stdout=stdout, stderr=stderr, exit_code=exit_code)
