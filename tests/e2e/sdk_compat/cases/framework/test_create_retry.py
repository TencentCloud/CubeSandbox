# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pure-logic unit tests for the sandbox-create capacity-retry helper.

Marked ``framework`` so they run on every gate without a live environment
(see ``conftest.pytest_collection_modifyitems``).
"""

from __future__ import annotations

import pytest

from framework import create_retry
from framework.create_retry import (
    CAPACITY_ERROR_CODE,
    create_with_capacity_retry,
    is_capacity_error,
)

pytestmark = pytest.mark.framework


class _CapacityError(Exception):
    """Stand-in for the SDK error raised when the scheduler is out of capacity."""


class _CodedError(Exception):
    def __init__(self, message: str = "", *, code: object = None) -> None:
        super().__init__(message)
        if code is not None:
            self.code = code


class _ApiError(Exception):
    """Mimics the CubeSandbox SDK ``ApiError``: only an HTTP ``status_code``.

    The SDK does NOT surface CubeMaster's numeric RetCode (130597); the create
    failure arrives as an HTTP 500 whose message is CubeMaster's ``RetMsg``, so
    detection for that backend relies on the message markers.
    """

    def __init__(self, message: str, status_code: int) -> None:
        super().__init__(message)
        self.status_code = status_code


@pytest.mark.parametrize(
    "message",
    [
        "no more resource",
        "NO MORE RESOURCE",
        "scheduler: SelectNodesNoRes",
        "node is out of capacity",
        "insufficient capacity on cluster",
        f"scheduling failed: {CAPACITY_ERROR_CODE}",
    ],
)
def test_is_capacity_error_matches_transient(message: str) -> None:
    assert is_capacity_error(Exception(message)) is True


@pytest.mark.parametrize(
    "message",
    [
        "invalid template id",
        "unauthorized",
        "resource exhausted",  # generic gRPC rate-limit, deliberately not retried
        "connection refused",
        "",
    ],
)
def test_is_capacity_error_ignores_other_errors(message: str) -> None:
    assert is_capacity_error(Exception(message)) is False


def test_is_capacity_error_matches_numeric_code_attribute() -> None:
    assert is_capacity_error(_CodedError("boom", code=int(CAPACITY_ERROR_CODE))) is True
    assert is_capacity_error(_CodedError("boom", code=CAPACITY_ERROR_CODE)) is True


def test_is_capacity_error_ignores_unrelated_code() -> None:
    assert is_capacity_error(_CodedError("boom", code=404)) is False


def test_is_capacity_error_matches_real_apierror_shape() -> None:
    # The primary backend's ApiError has no capacity code, only status_code=500
    # and CubeMaster's RetMsg text: detection must still trip on the message.
    exc = _ApiError("create sandbox failed: no more resource", status_code=500)
    assert is_capacity_error(exc) is True


# The exact ApiError message the SDK raises on a saturated create, traced
# through source (see create_retry module docstring): scheduler ErrNoRes (code
# 130597) -> CubeMasterError::Api Display -> ApiError(500, message). Pinning the
# real body keeps detection verified against the actual wire text, not an
# assumed wording; note it carries BOTH the code token and the phrase.
_REAL_SATURATED_CREATE_MESSAGE = (
    "CubeMaster returned error code 130597: no more resource"
)


def test_is_capacity_error_matches_real_wire_message() -> None:
    exc = _ApiError(_REAL_SATURATED_CREATE_MESSAGE, status_code=500)
    assert is_capacity_error(exc) is True
    # Even if the phrase wording ever drifts, the standalone 130597 token in the
    # same message keeps detection tripping.
    assert is_capacity_error(_ApiError("error code 130597", status_code=500)) is True


def test_is_capacity_error_ignores_status_code_500() -> None:
    # A generic 500 without capacity wording must not be retried.
    exc = _ApiError("internal server error", status_code=500)
    assert is_capacity_error(exc) is False


def test_is_capacity_error_requires_code_word_boundary() -> None:
    # 130597 embedded in a larger id must NOT be misread as the capacity code.
    assert is_capacity_error(Exception("sandbox sbx-1305971 failed")) is False
    assert is_capacity_error(Exception("retcode=130597 no schedulable node")) is True


def test_backoff_delay_grows_and_is_capped(monkeypatch: pytest.MonkeyPatch) -> None:
    # random.uniform(0, ceiling) -> return the ceiling so growth is observable.
    monkeypatch.setattr(create_retry.random, "uniform", lambda _lo, hi: hi)
    assert create_retry._backoff_delay(1, backoff=2, backoff_max=30) == 2
    assert create_retry._backoff_delay(2, backoff=2, backoff_max=30) == 4
    assert create_retry._backoff_delay(3, backoff=2, backoff_max=30) == 8
    assert create_retry._backoff_delay(5, backoff=2, backoff_max=30) == 30  # capped


def test_backoff_delay_uncapped_when_max_non_positive(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(create_retry.random, "uniform", lambda _lo, hi: hi)
    assert create_retry._backoff_delay(4, backoff=1, backoff_max=0) == 8


def test_backoff_delay_stays_within_ceiling() -> None:
    for attempt in range(1, 6):
        delay = create_retry._backoff_delay(attempt, backoff=2, backoff_max=30)
        assert 0.0 <= delay <= 30


def test_backoff_delay_uncapped_overflow_clamps_to_finite_ceiling(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Uncapped (backoff_max<=0) + huge backoff overflows to inf; instead of a
    # tight 0.0 loop the ceiling is clamped to the finite _UNCAPPED_BACKOFF_CEILING.
    monkeypatch.setattr(create_retry.random, "uniform", lambda _lo, hi: hi)
    delay = create_retry._backoff_delay(40, backoff=1e308, backoff_max=0)
    assert delay == create_retry._UNCAPPED_BACKOFF_CEILING


def test_backoff_delay_uncapped_grows_within_finite_ceiling() -> None:
    # A large-but-finite uncapped delay stays within the internal ceiling.
    for attempt in range(1, 40):
        delay = create_retry._backoff_delay(attempt, backoff=2, backoff_max=0)
        assert 0.0 <= delay <= create_retry._UNCAPPED_BACKOFF_CEILING


def _no_sleep(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(create_retry.time, "sleep", lambda _seconds: None)
    monkeypatch.setattr(create_retry.random, "uniform", lambda _lo, _hi: 0.0)


def test_returns_on_first_success(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        return "ok"

    assert create_with_capacity_retry(
        create, retries=5, backoff=2, backoff_max=30
    ) == "ok"
    assert calls["n"] == 1


def test_retries_capacity_error_then_succeeds(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    attempts = iter([Exception("no more resource"), Exception("no more resource")])

    def create() -> str:
        exc = next(attempts, None)
        if exc is not None:
            raise exc
        return "ok"

    seen: list[int] = []
    result = create_with_capacity_retry(
        create,
        retries=5,
        backoff=2,
        backoff_max=30,
        on_retry=lambda attempt, delay, exc: seen.append(attempt),
    )
    assert result == "ok"
    assert seen == [1, 2]  # 1-based attempt numbers


def test_non_capacity_error_raises_immediately(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise ValueError("bad template")

    with pytest.raises(ValueError):
        create_with_capacity_retry(create, retries=5, backoff=2, backoff_max=30)
    assert calls["n"] == 1  # no retry


def test_raises_last_capacity_error_when_exhausted(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise _CapacityError(f"no more resource #{calls['n']}")

    with pytest.raises(Exception, match="no more resource #3"):
        create_with_capacity_retry(create, retries=2, backoff=2, backoff_max=30)
    assert calls["n"] == 3  # first attempt + 2 retries


def test_retries_zero_is_single_attempt(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise _CapacityError("no more resource")

    with pytest.raises(Exception, match="no more resource"):
        create_with_capacity_retry(create, retries=0, backoff=2, backoff_max=30)
    assert calls["n"] == 1


def test_negative_retries_treated_as_zero(monkeypatch: pytest.MonkeyPatch) -> None:
    _no_sleep(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise _CapacityError("no more resource")

    with pytest.raises(Exception, match="no more resource"):
        create_with_capacity_retry(create, retries=-3, backoff=2, backoff_max=30)
    assert calls["n"] == 1


def _deterministic_delay(monkeypatch: pytest.MonkeyPatch) -> list[float]:
    # Return the full ceiling as the delay (no jitter) and record every sleep.
    slept: list[float] = []
    monkeypatch.setattr(create_retry.random, "uniform", lambda _lo, hi: hi)
    monkeypatch.setattr(create_retry.time, "sleep", lambda seconds: slept.append(seconds))
    return slept


def test_total_budget_stops_before_exceeding(monkeypatch: pytest.MonkeyPatch) -> None:
    # Delays are 2, 4, 8, ... With a 5s budget, the loop sleeps 2 (total 2),
    # then the next 4 would push total to 6 > 5, so it re-raises instead.
    slept = _deterministic_delay(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise _CapacityError("no more resource")

    with pytest.raises(Exception, match="no more resource"):
        create_with_capacity_retry(
            create, retries=10, backoff=2, backoff_max=30, total_budget=5
        )
    assert slept == [2]  # only the first backoff fit within the budget
    assert calls["n"] == 2  # first attempt + one retry, then budget stops it


def test_total_budget_disabled_when_non_positive(monkeypatch: pytest.MonkeyPatch) -> None:
    # total_budget<=0 keeps the retries-only behaviour (no wall-time cap).
    slept = _deterministic_delay(monkeypatch)
    calls = {"n": 0}

    def create() -> str:
        calls["n"] += 1
        raise _CapacityError("no more resource")

    with pytest.raises(Exception, match="no more resource"):
        create_with_capacity_retry(
            create, retries=3, backoff=2, backoff_max=30, total_budget=0
        )
    assert calls["n"] == 4  # first attempt + 3 retries, budget did not intervene
    assert slept == [2, 4, 8]
