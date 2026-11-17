# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic checks for restart-policy status parsing and the opt-in switch."""

from __future__ import annotations

import ast

import pytest

from framework.restart_policy import (
    CRASH_COMMAND,
    EXIT_ERROR_LAUNCH,
    EXIT_ERROR_PATH,
    EXIT_ERROR_SCRIPT,
    EXIT_ZERO_COMMAND,
    restart_status,
    signal_guest,
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


class _SignalAdapter:
    def __init__(self, exit_code):
        self.exit_code = exit_code

    def run_command(self, command, *, timeout):
        return type("Result", (), {"exit_code": self.exit_code})()


@pytest.mark.parametrize("exit_code", [-1, 0, None])
def test_signal_guest_accepts_dropped_channel(exit_code) -> None:
    signal_guest(_SignalAdapter(exit_code), "signal", timeout=1)


@pytest.mark.parametrize("exit_code", [128, 137, 143])
def test_signal_guest_accepts_signaled_channel_when_allowed(exit_code) -> None:
    signal_guest(
        _SignalAdapter(exit_code),
        "signal",
        timeout=1,
        allow_signaled=True,
    )


def test_signal_guest_rejects_unexpected_signaled_channel() -> None:
    with pytest.raises(AssertionError, match="exit=137"):
        signal_guest(_SignalAdapter(137), "signal", timeout=1)


def test_exit_error_script_calls_exit_group_on_supported_architectures() -> None:
    ast.parse(EXIT_ERROR_SCRIPT)
    assert 'machine in ("x86_64", "amd64")' in EXIT_ERROR_SCRIPT
    assert "regs.rax = 231" in EXIT_ERROR_SCRIPT
    assert "regs.rdi = 1" in EXIT_ERROR_SCRIPT
    assert 'machine in ("aarch64", "arm64")' in EXIT_ERROR_SCRIPT
    assert "regs.regs[8] = 94" in EXIT_ERROR_SCRIPT
    assert "regs.regs[0] = 1" in EXIT_ERROR_SCRIPT
    assert "0xD4000001" in EXIT_ERROR_SCRIPT
    assert "unsupported guest architecture" in EXIT_ERROR_SCRIPT
    assert EXIT_ERROR_LAUNCH == f"python3 {EXIT_ERROR_PATH}"
