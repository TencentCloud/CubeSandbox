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
        execution = self._sandbox.run_code(cmd, language="sh", timeout=timeout)
        stdout = "\n".join(execution.logs.stdout)
        stderr = "\n".join(execution.logs.stderr)
        exit_code = 1 if execution.error else 0
        return CommandResult(stdout=stdout, stderr=stderr, exit_code=exit_code)
