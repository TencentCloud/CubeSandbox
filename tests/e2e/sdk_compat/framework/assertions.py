# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from framework.models import CodeResult, CommandResult


def assert_command_ok(result: CommandResult) -> None:
    assert result.exit_code == 0, (
        f"expected command exit 0, got {result.exit_code}; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def assert_code_ok(result: CodeResult) -> None:
    assert result.error is None, (
        f"expected successful code execution, got error={result.error!r}; "
        f"text={result.text!r} stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def assert_stdout_contains(result: CommandResult, expected: str) -> None:
    assert expected in result.stdout, (
        f"expected stdout to contain {expected!r}, got stdout={result.stdout!r} stderr={result.stderr!r}"
    )
