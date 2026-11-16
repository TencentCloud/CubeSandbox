# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic tests for E2B adapter lifecycle normalization."""

from __future__ import annotations

import sys
from types import ModuleType, SimpleNamespace

import pytest
from adapters.e2b_adapter import E2BAdapter
from framework.exceptions import UnsupportedCapability

pytestmark = pytest.mark.framework


class _Sandbox:
    sandbox_id = "sbx-test"

    def __init__(self, *, state: str = "running", running: bool = True) -> None:
        self.state = state
        self.running = running
        self.get_info_calls = 0
        self.is_running_calls = 0

    def get_info(self):
        self.get_info_calls += 1
        return SimpleNamespace(sandbox_id=self.sandbox_id, state=self.state)

    def is_running(self) -> bool:
        self.is_running_calls += 1
        return self.running


class _NotFoundError(Exception):
    status_code = 404


class _SandboxNotFoundError(Exception):
    pass


@pytest.mark.parametrize(
    "state",
    ["paused", "pausing", "PAUSED", "killing", "killed", "terminated", "stopped"],
)
def test_is_running_returns_false_for_non_running_state_without_health_probe(
    state: str,
) -> None:
    sandbox = _Sandbox(state=state)

    assert E2BAdapter(sandbox, e2e_config=SimpleNamespace()).is_running() is False
    assert sandbox.get_info_calls == 1
    assert sandbox.is_running_calls == 0


def test_is_running_returns_false_for_http_not_found() -> None:
    sandbox = _Sandbox()

    def not_found():
        raise _NotFoundError("sandbox not found")

    sandbox.get_info = not_found

    assert E2BAdapter(sandbox, e2e_config=SimpleNamespace()).is_running() is False
    assert sandbox.is_running_calls == 0


def test_is_running_returns_false_for_e2b_not_found(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    e2b_module = ModuleType("e2b")
    exceptions_module = ModuleType("e2b.exceptions")
    exceptions_module.SandboxNotFoundException = _SandboxNotFoundError
    e2b_module.exceptions = exceptions_module
    monkeypatch.setitem(sys.modules, "e2b", e2b_module)
    monkeypatch.setitem(sys.modules, "e2b.exceptions", exceptions_module)

    sandbox = _Sandbox()

    def not_found():
        raise _SandboxNotFoundError("sandbox not found")

    sandbox.get_info = not_found

    assert E2BAdapter(sandbox, e2e_config=SimpleNamespace()).is_running() is False
    assert sandbox.is_running_calls == 0


@pytest.mark.parametrize("state", ["running", "unknown"])
def test_is_running_delegates_health_probe_for_probeable_state(state: str) -> None:
    sandbox = _Sandbox(state=state, running=True)

    assert E2BAdapter(sandbox, e2e_config=SimpleNamespace()).is_running() is True
    assert sandbox.get_info_calls == 1
    assert sandbox.is_running_calls == 1


def test_is_running_without_config_still_uses_sdk_control_plane() -> None:
    sandbox = _Sandbox(state="paused")

    assert E2BAdapter(sandbox).is_running() is False
    assert sandbox.get_info_calls == 1
    assert sandbox.is_running_calls == 0


def test_is_running_propagates_non_not_found_control_plane_error() -> None:
    sandbox = _Sandbox()

    def unavailable():
        raise RuntimeError("service unavailable")

    sandbox.get_info = unavailable

    with pytest.raises(RuntimeError, match="service unavailable"):
        E2BAdapter(sandbox).is_running()
    assert sandbox.is_running_calls == 0


def test_is_running_checks_capability_before_control_plane() -> None:
    get_info_calls = 0

    def get_info():
        nonlocal get_info_calls
        get_info_calls += 1
        return None

    sandbox = SimpleNamespace(sandbox_id="sbx-test", get_info=get_info)

    with pytest.raises(UnsupportedCapability):
        E2BAdapter(sandbox, e2e_config=SimpleNamespace()).is_running()
    assert get_info_calls == 0
