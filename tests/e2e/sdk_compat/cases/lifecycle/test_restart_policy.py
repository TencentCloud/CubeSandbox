# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Node-local restart policy cases.

These cover the user-visible rules: exit 0 restarts only Always, a non-zero
main-process exit restarts OnFailure and Always, Never does not restart,
backoff uses the configured interval, multiplier and cap, a stable period
clears the streak, pause during backoff abandons that restart, and resume
clears the count. Host-side scenarios (killing the hypervisor, restarting
cubelet, CubeMaster down) are not driven from this suite.

CubeMaster must have enable_restart_policy set, and the template must expose
envd on port 49983 so the default liveness probe can see a guest crash.
"""

from __future__ import annotations

import os
import time
import urllib.request
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime

import pytest
from adapters import create_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS, PAUSE_RESUME, RESTART_POLICY, capabilities_for_backend
from framework.create_retry import create_with_capacity_retry
from framework.lifecycle import (
    assert_connect_fails,
    fetch_state,
    sandbox_listed,
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)
from framework.restart_policy import (
    MARKER_PATH,
    crash_guest,
    exit_main_error,
    exit_main_zero,
    guest_ipv4,
    read_marker,
    restart_status,
    stop_liveness_endpoint,
    wait_for_restart_status,
    write_durable_marker,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.restart_policy,
    pytest.mark.requires_capability(RESTART_POLICY),
]

_FAST_BACKOFF = {
    "initialIntervalSecond": 1,
    "multiplier": 1,
    "maxRestarts": 5,
}
_ONE_RESTART = {
    "initialIntervalSecond": 1,
    "multiplier": 1,
    "maxRestarts": 1,
}
# initial * multiplier^count, then cap. jitter 0.1 matches the default.
# Small numbers keep the run short; the assertions still prove the interval
# starts at the configured value, grows by the multiplier and stops at the cap.
_SCHEDULE = {
    "initialIntervalSecond": 3,
    "multiplier": 4,
    "maxIntervalSecond": 6,
    "maxRestarts": 5,
    "jitter": 0.1,
}
_STABLE = {
    "initialIntervalSecond": 3,
    "multiplier": 4,
    "maxIntervalSecond": 6,
    "maxRestarts": 5,
    "jitter": 0.1,
    "stableDurationSecond": 8,
}
# Long enough that delete can land while the sandbox is still waiting.
_HOLD_BACKOFF = {
    "initialIntervalSecond": 20,
    "multiplier": 1,
    "maxRestarts": 5,
    "jitter": 0.1,
}
# Pause itself takes part of the window. The guest must still be up, so the
# wait has to outlast snapshotting the running VM (~10s in practice).
_PAUSE_BACKOFF = {
    "initialIntervalSecond": 30,
    "multiplier": 1,
    "maxRestarts": 5,
    "jitter": 0.1,
}
# A closed port fails the probe without killing envd or the main process, so
# the sandbox can be snapshotted while it is waiting to restart.
_CLOSED_PORT_PROBE = {
    "httpGet": {"path": "/", "port": 1},
    "initialDelaySecond": 1,
    "periodSecond": 1,
    "failureThreshold": 1,
    "probeTimeoutSecond": 1,
}
# The cases below exercise the restart decision, not the default 30s + 3 * 10s
# detection window, so they pin a probe that notices the failure in about a
# second. The probe still targets the real envd health endpoint.
_FAST_PROBE = {
    "httpGet": {"path": "/health", "port": 49983},
    "initialDelaySecond": 1,
    "periodSecond": 1,
    "failureThreshold": 1,
    "probeTimeoutSecond": 1,
}
# After the terminal state is visible, a wrongful restart would already be in
# BackOff, so a short hold proves the sandbox stays down.
_TERMINAL_HOLD_SECONDS = 15
# Past the default probe window (30s initial delay + 3 * 10s period) so the run
# covers a full detection cycle with the default probe.
_HEALTHY_WINDOW_SECONDS = 62
_KILL_WINDOW_SECONDS = 20
_MARKER = "cube-e2e-restart-marker"


@pytest.fixture(autouse=True)
def _restart_policy_gate(sdk_backend: str) -> None:
    if RESTART_POLICY not in capabilities_for_backend(sdk_backend):
        pytest.skip(
            f"backend {sdk_backend!r} does not support capability {RESTART_POLICY!r}"
        )


def _count(status: dict | None) -> int:
    if not status:
        return 0
    return int(status.get("restartCount") or 0)


def _exit_code(status: dict | None) -> int | None:
    if not status or status.get("lastExitCode") is None:
        return None
    return int(status["lastExitCode"])


def _parse_time(raw: str) -> datetime:
    target = datetime.fromisoformat(str(raw).replace("Z", "+00:00"))
    if target.tzinfo is None:
        target = target.replace(tzinfo=timezone.utc)
    return target


def _api_now(config) -> datetime:
    """Clock of the API that published nextRestartAt. The runner clock can differ."""
    req = urllib.request.Request(config.cube_api_url.rstrip("/") + "/health")
    api_key = os.environ.get("CUBE_API_KEY")
    if api_key:
        req.add_header("X-API-Key", api_key)
    with urllib.request.urlopen(req, timeout=20) as resp:
        date = resp.headers.get("Date")
    assert date, "API response has no Date header"
    parsed = parsedate_to_datetime(date)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def _seconds_until(status: dict | None, *, now: datetime) -> float | None:
    raw = None if not status else status.get("nextRestartAt")
    if not raw:
        return None
    return (_parse_time(str(raw)) - now).total_seconds()


def _assert_backoff(
    status: dict | None,
    *,
    config,
    initial: float,
    multiplier: float,
    cap: float,
    jitter: float,
    slack: float = 4,
) -> None:
    """Delay is initial * multiplier^count, capped, plus up to jitter.

    nextRestartAt is compared with the API Date header. That header is whole
    seconds, so the upper bound allows two extra seconds.
    """
    count = _count(status)
    raw = float(initial)
    if count > 0 and multiplier > 0:
        raw = float(initial) * (float(multiplier) ** count)
    if cap > 0 and raw > cap:
        raw = float(cap)
    now = _api_now(config)
    remaining = _seconds_until(status, now=now)
    high = raw * (1 + jitter) + 2
    assert remaining is not None and raw - slack <= remaining <= high, (
        f"backoff delay={raw}s remaining={remaining} now={now.isoformat()} status={status!r}"
    )


def _assert_rejected(err: BaseException, message: str, *, status_code: int | None = None) -> None:
    status = getattr(err, "status_code", None)
    assert message in str(err), f"status={status} error={err!r}"
    if status_code is None:
        assert status is not None and 400 <= int(status) < 500, (
            f"expected HTTP 4xx, got status={status} error={err!r}"
        )
        return
    assert status == status_code, (
        f"expected HTTP {status_code}, got status={status} error={err!r}"
    )


def _create_must_fail(
    sdk_backend: str,
    sdk_e2e_config,
    create_options: dict,
    message: str,
    *,
    status_code: int | None = None,
) -> None:
    try:
        adapter = create_with_capacity_retry(
            lambda: create_adapter(
                sdk_backend,
                sdk_e2e_config,
                create_options=create_options,
            ),
            retries=sdk_e2e_config.create_capacity_retries,
            backoff=sdk_e2e_config.create_capacity_backoff,
            backoff_max=sdk_e2e_config.create_capacity_backoff_max,
            total_budget=sdk_e2e_config.create_capacity_budget,
        )
    except Exception as exc:
        _assert_rejected(exc, message, status_code=status_code)
        return
    try:
        adapter.kill()
    finally:
        adapter.close()
    raise AssertionError(f"create succeeded; expected rejection containing {message!r}")


@pytest.mark.p1
@pytest.mark.parametrize(
    ("policy", "expected"),
    [
        pytest.param(
            "OnFailure",
            "OnFailure",
            marks=pytest.mark.sandbox_create_options(restartPolicy="OnFailure", timeout=600),
        ),
        pytest.param(
            "Always",
            "Always",
            marks=pytest.mark.sandbox_create_options(restartPolicy="Always", timeout=600),
        ),
        pytest.param(
            "RESTART_POLICY_ON_FAILURE",
            "OnFailure",
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="RESTART_POLICY_ON_FAILURE",
                timeout=600,
            ),
        ),
    ],
)
def test_restart_policy_visible_in_info(sdk_sandbox, sdk_e2e_config, policy, expected):
    status = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartPolicy") == expected,
        timeout=min(90, sdk_e2e_config.restart_policy_wait),
    )
    assert status is not None
    assert status.get("restartState") == "Running", (
        f"sandbox={sdk_sandbox.sandbox_id} policy={policy} status={status!r}"
    )
    assert _count(status) == 0, (
        f"sandbox={sdk_sandbox.sandbox_id} policy={policy} status={status!r}"
    )


@pytest.mark.p1
def test_default_restart_policy_is_never(sdk_sandbox):
    deadline = time.monotonic() + 15
    while True:
        status = restart_status(sdk_sandbox.info().raw)
        if status is not None:
            assert status.get("restartPolicy") in (None, "", "Never"), (
                f"sandbox={sdk_sandbox.sandbox_id} status={status!r}"
            )
            assert _count(status) == 0, (
                f"sandbox={sdk_sandbox.sandbox_id} status={status!r}"
            )
        if time.monotonic() >= deadline:
            return
        time.sleep(2)


@pytest.mark.p1
def test_invalid_restart_policy_rejected(sdk_backend, sdk_e2e_config):
    _create_must_fail(
        sdk_backend,
        sdk_e2e_config,
        {"restartPolicy": "nope"},
        "invalid restart policy",
    )


@pytest.mark.p1
def test_never_with_liveness_probe_rejected(sdk_backend, sdk_e2e_config):
    """Never + a liveness probe is refused: the probe could only kill the sandbox."""
    _create_must_fail(
        sdk_backend,
        sdk_e2e_config,
        {
            "restartPolicy": "Never",
            "livenessProbe": {"httpGet": {"path": "/health", "port": 49983}},
        },
        "liveness_probe requires restart policy",
        status_code=400,
    )


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.sandbox_create_options(restartPolicy="OnFailure", timeout=600)
def test_healthy_sandbox_not_restarted(sdk_sandbox, sdk_e2e_config):
    """A healthy sandbox with the default probe never restarts.

    The window covers the default 30s initial delay plus three 10s failures.
    """
    deadline = time.monotonic() + _HEALTHY_WINDOW_SECONDS
    while time.monotonic() < deadline:
        status = restart_status(sdk_sandbox.info().raw)
        assert _count(status) == 0, (
            f"healthy sandbox restarted: sandbox={sdk_sandbox.sandbox_id} status={status!r}"
        )
        if status is not None:
            assert status.get("restartState") in (None, "", "Running"), (
                f"sandbox={sdk_sandbox.sandbox_id} status={status!r}"
            )
        time.sleep(5)
    result = sdk_sandbox.run_command("printf healthy", timeout=sdk_e2e_config.command_timeout)
    assert_command_ok(result)
    assert result.stdout == "healthy"


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.parametrize(
    "policy",
    [
        pytest.param(
            "OnFailure",
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="OnFailure",
                restartBackoff=_FAST_BACKOFF,
                livenessProbe=_FAST_PROBE,
                timeout=1800,
            ),
        ),
        pytest.param(
            "Always",
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="Always",
                restartBackoff=_FAST_BACKOFF,
                livenessProbe=_FAST_PROBE,
                timeout=1800,
            ),
        ),
    ],
)
def test_guest_crash_restarts_same_sandbox(sdk_sandbox, sdk_e2e_config, policy):
    """A guest crash restarts under OnFailure and Always, same id and disk.

    The sysrq panic surfaces as a shim exit without a status, so the reason is
    the abnormal exit reported as Error and the exit code is -1.
    """
    sandbox_id = sdk_sandbox.sandbox_id
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    status = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "Running"
        and _count(item) >= 1,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    assert sdk_sandbox.sandbox_id == sandbox_id
    assert status is not None
    assert status.get("lastExitReason") not in (None, "", "Completed"), (
        f"sandbox={sandbox_id} policy={policy} status={status!r}"
    )
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    assert read_marker(sdk_sandbox, timeout=sdk_e2e_config.command_timeout) == _MARKER, (
        f"sandbox={sandbox_id} lost {MARKER_PATH} status={status!r}"
    )


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.sandbox_create_options(restartPolicy="OnFailure", timeout=600)
def test_kill_does_not_restart(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id
    sdk_sandbox.kill()
    deadline = time.monotonic() + _KILL_WINDOW_SECONDS
    while time.monotonic() < deadline:
        assert sandbox_listed(sandbox_id, sdk_backend, sdk_e2e_config) is not True, (
            f"killed sandbox {sandbox_id} was listed again"
        )
        time.sleep(2)
    failure = assert_connect_fails(sandbox_id, sdk_backend, sdk_e2e_config)
    assert failure, f"sandbox={sandbox_id} still accepts commands after kill"
    assert sandbox_listed(sandbox_id, sdk_backend, sdk_e2e_config) is not True


@pytest.mark.p3
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_ONE_RESTART,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_max_restarts_gives_up(sdk_sandbox, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    first = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "Running"
        and _count(item) == 1,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    final = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "GaveUp",
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    assert final is not None
    assert _count(final) == 1, (
        f"sandbox={sandbox_id} first={first!r} final={final!r}"
    )


def _assert_restarted(sdk_sandbox, sdk_e2e_config, sandbox_id: str, address: str, *, reason: str, code: int, count: int = 1):
    status = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "Running"
        and _count(item) == count
        and item.get("lastExitReason") == reason
        and _exit_code(item) == code,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    assert sdk_sandbox.sandbox_id == sandbox_id
    assert status is not None
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    assert guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout) == address, (
        f"sandbox={sandbox_id} status={status!r}"
    )
    assert read_marker(sdk_sandbox, timeout=sdk_e2e_config.command_timeout) == _MARKER, (
        f"sandbox={sandbox_id} lost {MARKER_PATH} status={status!r}"
    )
    return status


def _assert_stays_stopped(sdk_sandbox, sdk_e2e_config, *, state: str, reason: str, code: int):
    status = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == state
        and item.get("lastExitReason") == reason
        and _exit_code(item) == code
        and _count(item) == 0,
        timeout=min(90, sdk_e2e_config.restart_policy_wait),
    )
    deadline = time.monotonic() + _TERMINAL_HOLD_SECONDS
    while time.monotonic() < deadline:
        try:
            seen = restart_status(sdk_sandbox.info().raw)
        except Exception:
            seen = None
        if seen is not None:
            assert seen.get("restartState") == state, (
                f"sandbox={sdk_sandbox.sandbox_id} status={seen!r}"
            )
            assert _count(seen) == 0, (
                f"sandbox={sdk_sandbox.sandbox_id} status={seen!r}"
            )
        try:
            result = sdk_sandbox.run_command("true", timeout=5)
        except Exception:
            result = None
        if result is not None and getattr(result, "exit_code", 1) == 0:
            raise AssertionError(
                f"sandbox={sdk_sandbox.sandbox_id} came back after {state} status={seen!r}"
            )
        time.sleep(2)
    return status


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.parametrize(
    ("policy", "restarts"),
    [
        pytest.param(
            "Never",
            False,
            marks=pytest.mark.sandbox_create_options(restartPolicy="Never", timeout=1800),
        ),
        pytest.param(
            "OnFailure",
            False,
            marks=pytest.mark.sandbox_create_options(restartPolicy="OnFailure", timeout=1800),
        ),
        pytest.param(
            "Always",
            True,
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="Always",
                restartBackoff=_FAST_BACKOFF,
                timeout=1800,
            ),
        ),
    ],
)
def test_main_process_exit_zero(sdk_sandbox, sdk_e2e_config, policy, restarts):
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    address = guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    exit_main_zero(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    if restarts:
        _assert_restarted(
            sdk_sandbox,
            sdk_e2e_config,
            sandbox_id,
            address,
            reason="Completed",
            code=0,
        )
        return
    _assert_stays_stopped(
        sdk_sandbox,
        sdk_e2e_config,
        state="Succeeded",
        reason="Completed",
        code=0,
    )


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.parametrize(
    ("policy", "restarts"),
    [
        pytest.param(
            "Never",
            False,
            marks=pytest.mark.sandbox_create_options(restartPolicy="Never", timeout=1800),
        ),
        pytest.param(
            "OnFailure",
            True,
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="OnFailure",
                restartBackoff=_FAST_BACKOFF,
                timeout=1800,
            ),
        ),
        pytest.param(
            "Always",
            True,
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="Always",
                restartBackoff=_FAST_BACKOFF,
                timeout=1800,
            ),
        ),
    ],
)
def test_main_process_exit_nonzero(sdk_sandbox, sdk_e2e_config, policy, restarts):
    """Main process exit code 1. Never stays Failed; OnFailure and Always restart.

    The kernel does not deliver SIGKILL to a pid-namespace init, and uvicorn
    turns SIGTERM into exit 0. exit_group(1) is the abnormal main-process exit.
    """
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    address = guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    exit_main_error(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    if restarts:
        _assert_restarted(
            sdk_sandbox,
            sdk_e2e_config,
            sandbox_id,
            address,
            reason="Error",
            code=1,
        )
        return
    _assert_stays_stopped(
        sdk_sandbox,
        sdk_e2e_config,
        state="Failed",
        reason="Error",
        code=1,
    )


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_SCHEDULE,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_backoff_follows_configured_schedule(sdk_sandbox, sdk_e2e_config):
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    first = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "BackOff"
        and bool(item.get("nextRestartAt"))
        and _count(item) == 0,
        timeout=sdk_e2e_config.restart_policy_wait,
        interval=0.5,
    )
    _assert_backoff(first, config=sdk_e2e_config, initial=3, multiplier=4, cap=6, jitter=0.1)
    wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "Running" and _count(item) == 1,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    second = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "BackOff"
        and bool(item.get("nextRestartAt"))
        and _count(item) == 1,
        timeout=sdk_e2e_config.restart_policy_wait,
        interval=0.5,
    )
    _assert_backoff(second, config=sdk_e2e_config, initial=3, multiplier=4, cap=6, jitter=0.1)
    wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "Running" and _count(item) == 2,
        timeout=sdk_e2e_config.restart_policy_wait,
    )


@pytest.mark.p3
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_STABLE,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_stable_period_resets_count_and_backoff(sdk_sandbox, sdk_e2e_config):
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "Running" and _count(item) == 1,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    time.sleep(_STABLE["stableDurationSecond"] + 2)
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    reset = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "BackOff"
        and bool(item.get("nextRestartAt"))
        and _count(item) == 0,
        timeout=sdk_e2e_config.restart_policy_wait,
        interval=0.5,
    )
    _assert_backoff(reset, config=sdk_e2e_config, initial=3, multiplier=4, cap=6, jitter=0.1)
    final = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "Running" and _count(item) == 1,
        timeout=sdk_e2e_config.restart_policy_wait,
    )
    assert _count(final) == 1, f"sandbox={sdk_sandbox.sandbox_id} status={final!r}"


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_PAUSE_BACKOFF,
    livenessProbe=_CLOSED_PORT_PROBE,
    timeout=1800,
)
def test_pause_during_backoff_abandons_restart_and_resume_clears_count(sdk_sandbox, sdk_e2e_config):
    """Pause during the first backoff, before that restart has been created.

    The closed-port probe fails without killing the guest, so the original VM
    can be snapshotted and resumed. The restart has not completed, so the
    count is still 0; staying paused past nextRestartAt drops that attempt,
    and resume comes back with the count still 0.
    """
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    address = guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    waiting = wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "BackOff"
        and _count(item) == 0
        and bool(item.get("nextRestartAt")),
        timeout=sdk_e2e_config.restart_policy_wait,
        interval=0.5,
    )
    assert waiting is not None
    due = _parse_time(str(waiting["nextRestartAt"]))
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert _api_now(sdk_e2e_config) < due, (
        f"sandbox={sandbox_id} pause finished after the restart was due status={waiting!r}"
    )
    hold_until = due + timedelta(seconds=8)
    while _api_now(sdk_e2e_config) < hold_until:
        state = fetch_state(sdk_sandbox)
        assert state == "paused", (
            f"sandbox={sandbox_id} left paused during backoff state={state!r}"
        )
        time.sleep(2)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        status = wait_for_restart_status(
            resumed,
            lambda item: bool(item)
            and item.get("restartPolicy") == "OnFailure"
            and item.get("restartState") in ("Running", "BackOff")
            and _count(item) == 0,
            timeout=min(90, sdk_e2e_config.restart_policy_wait),
        )
        assert resumed.sandbox_id == sandbox_id
        assert status is not None
        assert guest_ipv4(resumed, timeout=sdk_e2e_config.command_timeout) == address
        assert read_marker(resumed, timeout=sdk_e2e_config.command_timeout) == _MARKER
    finally:
        resumed.close()


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_FAST_BACKOFF,
    timeout=1800,
)
def test_pause_and_resume_after_cold_restart(sdk_sandbox, sdk_e2e_config):
    """Pause and resume a sandbox that has already cold restarted once.

    A cold restart runs the guest main process under the sandbox id, so the
    pause snapshot records that id instead of the template's container id.
    Resume must reconnect to the restored process instead of starting it again.
    """
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    address = guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    exit_main_error(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    _assert_restarted(
        sdk_sandbox,
        sdk_e2e_config,
        sandbox_id,
        address,
        reason="Error",
        code=1,
    )
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        assert resumed.sandbox_id == sandbox_id
        status = restart_status(resumed.info().raw)
        assert _count(status) == 1, (
            f"sandbox={sandbox_id} lost the restart count after resume status={status!r}"
        )
        assert guest_ipv4(resumed, timeout=sdk_e2e_config.command_timeout) == address
        assert read_marker(resumed, timeout=sdk_e2e_config.command_timeout) == _MARKER
    finally:
        resumed.close()


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_HOLD_BACKOFF,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_delete_during_backoff_does_not_restart(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    crash_guest(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    wait_for_restart_status(
        sdk_sandbox,
        lambda item: bool(item) and item.get("restartState") == "BackOff",
        timeout=sdk_e2e_config.restart_policy_wait,
        interval=0.5,
    )
    sdk_sandbox.kill()
    deadline = time.monotonic() + _HOLD_BACKOFF["initialIntervalSecond"] + 10
    while time.monotonic() < deadline:
        assert sandbox_listed(sandbox_id, sdk_backend, sdk_e2e_config) is not True, (
            f"sandbox {sandbox_id} was listed again during backoff delete"
        )
        time.sleep(2)
    failure = assert_connect_fails(sandbox_id, sdk_backend, sdk_e2e_config)
    assert failure, f"sandbox={sandbox_id} still accepts commands after delete during backoff"


@pytest.mark.p2
@pytest.mark.slow
@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.parametrize(
    "policy",
    [
        pytest.param(
            "OnFailure",
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="OnFailure",
                restartBackoff=_FAST_BACKOFF,
                timeout=1800,
            ),
        ),
        pytest.param(
            "Always",
            marks=pytest.mark.sandbox_create_options(
                restartPolicy="Always",
                restartBackoff=_FAST_BACKOFF,
                timeout=1800,
            ),
        ),
    ],
)
def test_liveness_probe_failure_restarts(sdk_sandbox, sdk_e2e_config, policy):
    """Stop envd and let the default probe restart the sandbox.

    This case intentionally keeps the default liveness probe so it covers the
    30s initial delay + 3 * 10s detection window.
    """
    sandbox_id = sdk_sandbox.sandbox_id
    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    address = guest_ipv4(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    write_durable_marker(sdk_sandbox, _MARKER, timeout=sdk_e2e_config.command_timeout)
    stop_liveness_endpoint(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
    _assert_restarted(
        sdk_sandbox,
        sdk_e2e_config,
        sandbox_id,
        address,
        reason="LivenessProbeFailed",
        code=0,
    )
