# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic checks for restart-policy status parsing and the opt-in switch."""

from __future__ import annotations

import ast
from types import SimpleNamespace

import pytest

from framework.restart_policy import (
    CRASH_COMMAND,
    EXIT_ERROR_LAUNCH,
    EXIT_ERROR_PATH,
    EXIT_ERROR_SCRIPT,
    EXIT_ZERO_COMMAND,
    restart_status,
    signal_guest,
    wait_for_restart_status,
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


def test_restart_status_keeps_restart_timestamps() -> None:
    # restartCount alone cannot tell "two rebuilds" from "one rebuild plus a
    # failed attempt"; the timestamps are the only evidence, so keep them.
    got = restart_status(
        {
            "restartStatus": {
                "restartState": "Running",
                "restartCount": 2,
                "lastRestartAt": "2026-11-25T04:03:36Z",
                "lastSuccessfulRestartAt": "2026-11-25T04:03:42Z",
                "lastFailedRestartAt": "2026-11-25T04:03:35Z",
            }
        }
    )
    assert got is not None
    assert got["restartCount"] == 2
    assert got["lastRestartAt"] == "2026-11-25T04:03:36Z"
    assert got["lastSuccessfulRestartAt"] == "2026-11-25T04:03:42Z"
    assert got["lastFailedRestartAt"] == "2026-11-25T04:03:35Z"


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


class _ScriptedAdapter:
    """Replays a fixed status sequence, then repeats the last sample."""

    def __init__(self, statuses: list[dict]) -> None:
        self._statuses = statuses
        self._index = 0

    def info(self) -> SimpleNamespace:
        status = self._statuses[min(self._index, len(self._statuses) - 1)]
        self._index += 1
        return SimpleNamespace(raw={"restartStatus": status})


_CRASHED = {"lastExitReason": "Error", "lastExitCode": 1}


def _restarted(item, *, count: int = 1) -> bool:
    return (
        bool(item)
        and item.get("restartState") == "Running"
        and int(item.get("restartCount") or 0) >= count
        and item.get("lastExitReason") == "Error"
        and item.get("lastExitCode") == 1
    )


def test_wait_for_restart_status_accepts_a_charged_retry() -> None:
    # One exit, two attempts: the first rebuild failed, so it spent a retry too.
    adapter = _ScriptedAdapter(
        [
            {"restartState": "BackOff", "restartCount": 0, **_CRASHED},
            {"restartState": "Restarting", "restartCount": 0, **_CRASHED},
            {"restartState": "BackOff", "restartCount": 1, **_CRASHED},
            {"restartState": "Restarting", "restartCount": 1, **_CRASHED},
            {"restartState": "Running", "restartCount": 2, **_CRASHED},
        ]
    )
    got = wait_for_restart_status(
        adapter, _restarted, timeout=2, interval=0.001, hold=0.005
    )
    assert got is not None
    assert got["restartCount"] == 2


def test_wait_for_restart_status_requires_the_hold_window() -> None:
    # The rebuild succeeds, then the queued attempt immediately takes the
    # sandbox back to BackOff. Only the run it settles on is the answer.
    adapter = _ScriptedAdapter(
        [
            {"restartState": "Running", "restartCount": 1, **_CRASHED},
            {"restartState": "BackOff", "restartCount": 1, **_CRASHED},
            {"restartState": "Restarting", "restartCount": 1, **_CRASHED},
            {"restartState": "Running", "restartCount": 2, **_CRASHED},
        ]
    )
    got = wait_for_restart_status(
        adapter, _restarted, timeout=2, interval=0.001, hold=0.005
    )
    assert got is not None
    assert got["restartCount"] == 2


def test_exact_restart_count_would_time_out_on_a_charged_retry() -> None:
    # The invariant the case used to assert: one exit means exactly one counted
    # rebuild. A charged retry violates it, which is the e2e failure this covers.
    adapter = _ScriptedAdapter(
        [
            {"restartState": "BackOff", "restartCount": 1, **_CRASHED},
            {"restartState": "Running", "restartCount": 2, **_CRASHED},
        ]
    )
    with pytest.raises(AssertionError):
        wait_for_restart_status(
            adapter,
            lambda item: _restarted(item) and int(item.get("restartCount") or 0) == 1,
            timeout=0.05,
            interval=0.001,
        )
