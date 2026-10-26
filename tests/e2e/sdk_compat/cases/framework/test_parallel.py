# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pure-logic unit tests for the xdist worker-count resolver.

Marked ``framework`` so they run on every gate without a live environment
(see ``conftest.pytest_collection_modifyitems``).
"""

from __future__ import annotations

from unittest import mock

import pytest
from framework.build_throttle import _concurrency, template_build_slot
from framework.parallel import (
    apply_worker_count,
    current_worker_count,
    resolve_worker_count,
    scale_timeout_for_xdist,
    wants_parallel,
)

pytestmark = pytest.mark.framework


@pytest.mark.parametrize(
    ("env_value", "expected"),
    [
        # Parallelism is opt-in: unset/empty keeps the run serial.
        (None, None),
        ("", None),
        ("   ", None),
        ("0", None),
        ("1", None),
        ("no", None),
        ("off", None),
        ("OFF", None),
        (" No ", None),
        ("auto", "auto"),
        ("logical", "logical"),
        ("AUTO", "auto"),
        ("4", 4),
        ("16", 16),
        (" 3 ", 3),
    ],
)
def test_resolve_worker_count(env_value, expected):
    assert resolve_worker_count(env_value) == expected


@pytest.mark.parametrize("env_value", ["four", "2.5", "0x4"])
def test_resolve_worker_count_rejects_non_integer(env_value):
    with pytest.raises(ValueError, match="positive integer"):
        resolve_worker_count(env_value)


@pytest.mark.parametrize("env_value", ["-4", "-1"])
def test_resolve_worker_count_rejects_non_positive(env_value):
    with pytest.raises(ValueError, match=">= 1"):
        resolve_worker_count(env_value)


@pytest.mark.parametrize(
    ("env_value", "expected"),
    [
        # Unset/disabled = serial by choice, not a request for parallelism.
        (None, False),
        ("", False),
        ("0", False),
        ("1", False),
        ("off", False),
        # A real request (count or passthrough token) -- or a malformed value,
        # which counts as a request so the misconfig is surfaced, not swallowed.
        ("4", True),
        ("auto", True),
        ("four", True),
    ],
)
def test_wants_parallel(env_value, expected):
    assert wants_parallel(env_value) is expected


def _clear_xdist_env(monkeypatch):
    monkeypatch.delenv("PYTEST_XDIST_WORKER_COUNT", raising=False)
    monkeypatch.delenv("PYTEST_XDIST_WORKER", raising=False)


def test_current_worker_count_serial_default(monkeypatch):
    _clear_xdist_env(monkeypatch)
    assert current_worker_count() == 1


def test_current_worker_count_uses_authoritative_count(monkeypatch):
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "8")
    assert current_worker_count() == 8


@pytest.mark.parametrize("bad_count", ["nan", "0"])
def test_current_worker_count_falls_back_when_count_unusable(monkeypatch, bad_count):
    # Non-integer or < 1 authoritative value falls through to the host-stable
    # fallback, which every worker agrees on (no per-worker divergence).
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", bad_count)
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw3")
    monkeypatch.setattr("framework.parallel.os.cpu_count", lambda: 6)
    assert current_worker_count() == 6


@pytest.mark.parametrize("worker_name", ["gw0", "gw1", "gw3"])
def test_current_worker_count_fallback_is_worker_agnostic(monkeypatch, worker_name):
    # When the authoritative count is missing, every worker must resolve the same
    # count so timeout and retry scaling stay in sync.
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER", worker_name)
    monkeypatch.setattr("framework.parallel.os.cpu_count", lambda: 6)
    assert current_worker_count() == 6


def test_current_worker_count_fallback_when_cpu_count_unknown(monkeypatch):
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw0")
    monkeypatch.setattr("framework.parallel.os.cpu_count", lambda: None)
    assert current_worker_count() == 1


def test_scale_timeout_serial_is_unchanged(monkeypatch):
    _clear_xdist_env(monkeypatch)
    assert scale_timeout_for_xdist(120) == 120


def test_scale_timeout_widens_by_worker_count(monkeypatch):
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "4")
    assert scale_timeout_for_xdist(120) == 480


@pytest.mark.parametrize(
    ("env_value", "expected"),
    [
        (None, 1),
        ("", 1),
        ("   ", 1),
        ("3", 3),
        (" 2 ", 2),
        # Non-integer or < 1 falls back to the default of 1.
        ("nan", 1),
        ("0", 1),
        ("-2", 1),
    ],
)
def test_concurrency(monkeypatch, env_value, expected):
    if env_value is None:
        monkeypatch.delenv("SDK_E2E_TEMPLATE_BUILD_CONCURRENCY", raising=False)
    else:
        monkeypatch.setenv("SDK_E2E_TEMPLATE_BUILD_CONCURRENCY", env_value)
    assert _concurrency() == expected


class _StubOption:
    def __init__(self, numprocesses=None):
        self.numprocesses = numprocesses


class _StubPluginManager:
    def __init__(self, has_xdist=True):
        self._has_xdist = has_xdist

    def hasplugin(self, name):
        return name == "xdist" and self._has_xdist


class _StubConfig:
    """Minimal stand-in for ``pytest.Config`` for ``apply_worker_count``."""

    def __init__(self, *, run_e2e=True, has_xdist=True, numprocesses=None):
        self._run_e2e = run_e2e
        self.option = _StubOption(numprocesses)
        self.pluginmanager = _StubPluginManager(has_xdist)

    def getoption(self, name):
        assert name == "--run-e2e"
        return self._run_e2e


@pytest.fixture(autouse=True)
def _not_in_worker(monkeypatch):
    # apply_worker_count/the conftest hook short-circuit inside an xdist worker
    # (PYTEST_XDIST_WORKER set). These tests exercise the controller decision, so
    # clear it -- otherwise running the suite itself under -n would flip results.
    monkeypatch.delenv("PYTEST_XDIST_WORKER", raising=False)


def test_apply_worker_count_sets_numprocesses_from_env(monkeypatch):
    monkeypatch.setenv("SDK_E2E_WORKERS", "4")
    config = _StubConfig()
    assert apply_worker_count(config) == 4
    assert config.option.numprocesses == 4


def test_apply_worker_count_skips_inside_xdist_worker(monkeypatch):
    # In a worker process numprocesses is None and re-deriving it is useless and
    # misleading (would trigger a spurious per-worker serial warning). Stay out.
    monkeypatch.setenv("SDK_E2E_WORKERS", "4")
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw0")
    config = _StubConfig()
    assert apply_worker_count(config) is None
    assert config.option.numprocesses is None


def test_apply_worker_count_passthrough_token(monkeypatch):
    monkeypatch.setenv("SDK_E2E_WORKERS", "auto")
    config = _StubConfig()
    assert apply_worker_count(config) == "auto"
    assert config.option.numprocesses == "auto"


def test_apply_worker_count_serial_when_env_unset(monkeypatch):
    monkeypatch.delenv("SDK_E2E_WORKERS", raising=False)
    config = _StubConfig()
    assert apply_worker_count(config) is None
    assert config.option.numprocesses is None


def test_apply_worker_count_skips_without_run_e2e(monkeypatch):
    monkeypatch.setenv("SDK_E2E_WORKERS", "4")
    config = _StubConfig(run_e2e=False)
    assert apply_worker_count(config) is None
    assert config.option.numprocesses is None


def test_apply_worker_count_skips_when_xdist_absent(monkeypatch):
    monkeypatch.setenv("SDK_E2E_WORKERS", "4")
    config = _StubConfig(has_xdist=False)
    assert apply_worker_count(config) is None
    assert config.option.numprocesses is None


def test_apply_worker_count_respects_explicit_numprocesses(monkeypatch):
    # An explicit -n (even -n0) must win over SDK_E2E_WORKERS.
    monkeypatch.setenv("SDK_E2E_WORKERS", "4")
    config = _StubConfig(numprocesses=0)
    assert apply_worker_count(config) is None
    assert config.option.numprocesses == 0


def test_apply_worker_count_raises_on_malformed_env(monkeypatch):
    monkeypatch.setenv("SDK_E2E_WORKERS", "four")
    config = _StubConfig()
    with pytest.raises(ValueError, match="positive integer"):
        apply_worker_count(config)


def _force_throttle_active(monkeypatch):
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw0")
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "4")
    monkeypatch.delenv("SDK_E2E_TEMPLATE_BUILD_CONCURRENCY", raising=False)


def test_template_build_slot_degrades_when_acquire_fails(monkeypatch):
    # An OSError from slot acquisition (ELOOP/EACCES/ENOSPC) must degrade to a
    # yield-through no-op, not crash the build with an unrelated error.
    _force_throttle_active(monkeypatch)

    def _boom(_slots, _timeout):
        raise OSError("planted symlink at slot path")

    monkeypatch.setattr("framework.build_throttle._acquire_any_slot", _boom)
    entered = False
    with template_build_slot(label="unit"):
        entered = True
    assert entered


def test_template_build_slot_degrades_on_timeout(monkeypatch):
    # A wedged peer that never releases its slot must not stall this worker
    # forever: once the acquisition wait elapses (returns None), degrade to
    # unthrottled rather than blocking.
    _force_throttle_active(monkeypatch)

    def _timeout(_slots, _timeout):
        return None

    monkeypatch.setattr("framework.build_throttle._acquire_any_slot", _timeout)
    entered = False
    with template_build_slot(label="unit"):
        entered = True
    assert entered


def test_template_build_slot_serial_uses_acquire_path(monkeypatch, tmp_path):
    pytest.importorskip("fcntl")
    monkeypatch.delenv("PYTEST_XDIST_WORKER", raising=False)
    monkeypatch.delenv("PYTEST_XDIST_WORKER_COUNT", raising=False)
    monkeypatch.setattr("framework.build_throttle._LOCK_DIR", tmp_path / "locks")
    handle = mock.Mock()
    handle.fileno.return_value = -1

    def _acquire(_slots, _timeout):
        return handle, 0

    monkeypatch.setattr("framework.build_throttle._acquire_any_slot", _acquire)
    with template_build_slot(label="unit"):
        handle.close.assert_not_called()
    handle.close.assert_called_once_with()


def test_slot_wait_timeout_scales_with_workers(monkeypatch):
    monkeypatch.delenv("SDK_E2E_TEMPLATE_BUILD_WAIT", raising=False)
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "4")
    from framework.build_throttle import _DEFAULT_SLOT_WAIT_BASE, _slot_wait_timeout

    assert _slot_wait_timeout() == _DEFAULT_SLOT_WAIT_BASE * 4


def test_slot_wait_timeout_env_override(monkeypatch):
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "2")
    monkeypatch.setenv("SDK_E2E_TEMPLATE_BUILD_WAIT", "60")
    from framework.build_throttle import _slot_wait_timeout

    assert _slot_wait_timeout() == 120


def test_slot_wait_timeout_disabled_is_infinite(monkeypatch):
    monkeypatch.setenv("PYTEST_XDIST_WORKER_COUNT", "4")
    monkeypatch.setenv("SDK_E2E_TEMPLATE_BUILD_WAIT", "0")
    from framework.build_throttle import _slot_wait_timeout

    assert _slot_wait_timeout() == float("inf")


def test_acquire_any_slot_returns_none_on_timeout(monkeypatch, tmp_path):
    # Drive the real _acquire_any_slot with a slot already held so it must wait,
    # and a zero timeout so it gives up immediately and returns None.
    # _acquire_any_slot is POSIX-only (uses fcntl.flock); the module degrades to a
    # no-op without fcntl, so skip rather than error on a non-POSIX platform.
    fcntl = pytest.importorskip("fcntl")
    import framework.build_throttle as bt

    monkeypatch.setattr(bt, "_LOCK_DIR", tmp_path)
    monkeypatch.setattr(bt, "_POLL_INTERVAL", 0.01)
    holder, _idx = bt._acquire_any_slot(1)
    try:
        assert bt._acquire_any_slot(1, timeout=0.05) is None
    finally:
        fcntl.flock(holder.fileno(), fcntl.LOCK_UN)
        holder.close()


def test_current_worker_count_fallback_is_capped(monkeypatch):
    # A many-core box must not inflate the fallback multiplier without bound.
    _clear_xdist_env(monkeypatch)
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw0")
    monkeypatch.setattr("framework.parallel.os.cpu_count", lambda: 64)
    from framework.parallel import _MAX_FALLBACK_WORKERS

    assert current_worker_count() == _MAX_FALLBACK_WORKERS
