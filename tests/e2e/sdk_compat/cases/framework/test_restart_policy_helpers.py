# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic checks for restart-policy status parsing and the opt-in switch."""

from __future__ import annotations

import pytest

from framework.restart_policy import (
    CRASH_COMMAND,
    EXIT_ERROR_LAUNCH,
    EXIT_ERROR_SCRIPT,
    EXIT_ZERO_COMMAND,
    restart_status,
)

pytestmark = pytest.mark.framework


def test_restart_status_absent() -> None:
    assert restart_status({}) is None
    assert restart_status({"restartStatus": None}) is None
    assert restart_status({"restartStatus": {}}) is None
    assert restart_status("not-a-dict") is None


def test_restart_status_canonical_fields() -> None:
    got = restart_status(
        {
            "restartStatus": {
                "restartPolicy": "OnFailure",
                "restartState": "Running",
                "restartCount": 0,
                "lastExitReason": "",
                "lastExitCode": 0,
                "nextRestartAt": "2026-09-28T12:00:10Z",
            }
        }
    )
    assert got == {
        "restartPolicy": "OnFailure",
        "restartState": "Running",
        "restartCount": 0,
        "lastExitReason": "",
        "lastExitCode": 0,
        "nextRestartAt": "2026-09-28T12:00:10Z",
    }


def test_restart_status_snake_case_alias() -> None:
    got = restart_status(
        {
            "restart_status": {
                "restart_policy": "Always",
                "restart_state": "GaveUp",
                "restart_count": 1,
                "last_exit_reason": "LivenessProbeFailed",
            }
        }
    )
    assert got == {
        "restartPolicy": "Always",
        "restartState": "GaveUp",
        "restartCount": 1,
        "lastExitReason": "LivenessProbeFailed",
    }


def test_crash_command_returns_before_sysrq() -> None:
    assert "sleep 1" in CRASH_COMMAND
    assert "sysrq-trigger" in CRASH_COMMAND
    assert CRASH_COMMAND.rstrip().endswith("&")


def test_exit_zero_command_backgrounds_sigterm() -> None:
    assert "kill -TERM 1" in EXIT_ZERO_COMMAND
    assert EXIT_ZERO_COMMAND.rstrip().endswith("&")


def test_exit_error_script_calls_exit_group() -> None:
    assert "regs.rax = 231" in EXIT_ERROR_SCRIPT
    assert "regs.rdi = 1" in EXIT_ERROR_SCRIPT
    assert EXIT_ERROR_LAUNCH.rstrip().endswith("&")
