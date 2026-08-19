# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Retry sandbox creation when the scheduler is temporarily out of capacity.

A shared CI environment can transiently reject ``create`` calls when every node
is saturated. The scheduler reports this as ``no more resource`` (see
``CubeMaster/pkg/scheduler`` ``ErrNoRes`` / ``ErrorCode_SelectNodesNoRes``),
which the SDK surfaces as an ``ApiError``. That condition clears on its own once
another sandbox is torn down, so the E2E harness retries with exponential
backoff instead of failing the whole run.

Only capacity errors are retried; every other failure (bad template, auth,
``ImportError`` for a missing SDK, …) propagates immediately.

Verified wire shape (why the message markers below are reliable). The full path
of a saturated create, traced through source rather than assumed:

* CubeMaster scheduler raises ``ErrNoRes`` ("no more resource") with code
  ``ErrorCode_SelectNodesNoRes`` (130597), returned as
  ``Status{RetCode: 130597, RetMsg: "no more resource"}``.
* CubeAPI wraps that as ``CubeMasterError::Api`` whose ``Display`` is
  ``"CubeMaster returned error code 130597: no more resource"``
  (``CubeAPI/src/cubemaster/mod.rs``), then serialises it via
  ``AppError::Internal`` → ``Json(ApiError::new(500, e.to_string()))``
  (``CubeAPI/src/error/mod.rs``) into a JSON body ``{"code": 500,
  "message": "CubeMaster returned error code 130597: no more resource"}``.
* The SDK's ``_check_response`` reads ``body.get("message")``
  (``sdk/python/cubesandbox/sandbox.py``) and raises
  ``ApiError(message, status_code=500)``.

So the on-the-wire message carries BOTH the ``130597`` token and the
``no more resource`` text; detection trips on either. ``test_create_retry``
pins this exact string so the markers stay verified against the real body.
"""

from __future__ import annotations

import math
import random
import re
import time
from collections.abc import Callable
from typing import TypeVar

T = TypeVar("T")

# The scheduler's numeric code for "no schedulable node" (ErrNoRes /
# ErrorCode_SelectNodesNoRes). Note the CubeSandbox Python SDK's ``ApiError``
# only carries the HTTP ``status_code``, not this RetCode, so for that backend
# detection falls back to the message markers below; the code is checked mainly
# for SDKs/transports that do surface a numeric field, or that echo it in the
# message text.
CAPACITY_ERROR_CODE = "130597"

# Absolute finite ceiling (seconds) applied even when ``backoff_max`` is
# non-positive ("uncapped"). A misconfigured base (e.g. ``backoff=1e308``)
# overflows the exponential to ``inf``; without this bound the delay would
# either reach ``time.sleep`` as ``inf`` (raising) or, if forced to ``0.0``,
# collapse into a tight retry loop. Clamping to a large-but-finite value keeps
# an uncapped misconfiguration safe without silently disabling backoff.
_UNCAPPED_BACKOFF_CEILING = 3600.0
_CAPACITY_CODE_RE = re.compile(rf"\b{CAPACITY_ERROR_CODE}\b")

# Message substrings that identify a transient out-of-capacity failure. Matched
# case-insensitively; ``no more resource`` is CubeMaster's ``ErrNoRes.Error()``.
# Kept specific to avoid retrying unrelated errors (e.g. generic gRPC
# ``resource exhausted`` rate-limit responses).
_CAPACITY_MARKERS = (
    "no more resource",
    "selectnodesnores",
    "out of capacity",
    "insufficient capacity",
)


def is_capacity_error(exc: BaseException) -> bool:
    """Return ``True`` when ``exc`` looks like a transient out-of-capacity error."""
    code = getattr(exc, "code", None)
    if code is None:
        code = getattr(exc, "error_code", None)
    if code is not None and str(code) == CAPACITY_ERROR_CODE:
        return True
    message = str(exc).lower()
    if _CAPACITY_CODE_RE.search(message):
        return True
    return any(marker in message for marker in _CAPACITY_MARKERS)


def create_with_capacity_retry(
    create: Callable[[], T],
    *,
    retries: int,
    backoff: float,
    backoff_max: float,
    total_budget: float = 0.0,
    on_retry: Callable[[int, float, BaseException], None] | None = None,
) -> T:
    """Call ``create`` and retry it while the scheduler is out of capacity.

    Args:
        create: Zero-argument callable that creates and returns the sandbox
            adapter. Invoked once per attempt.
        retries: Maximum number of *additional* attempts after the first one.
            ``0`` disables retrying (a single attempt). Negative values are
            treated as ``0``.
        backoff: Base delay, in seconds, for the first retry. Each subsequent
            retry doubles the ceiling (exponential backoff) and the actual delay
            is drawn uniformly in ``[0, ceiling]`` (full jitter) so parallel
            workers do not retry in lockstep.
        backoff_max: Upper bound, in seconds, for any single backoff delay.
            ``<= 0`` disables the per-delay cap (delays then grow up to the
            internal ``_UNCAPPED_BACKOFF_CEILING``), it does NOT mean "zero
            backoff".
        total_budget: Optional wall-clock budget, in seconds, for the whole
            retry loop (sleeps only; the per-attempt ``create`` timeout is
            owned by the caller). ``<= 0`` disables it. When the accumulated
            backoff sleep would exceed the budget, the loop stops early and
            re-raises the last capacity error instead of sleeping again. This
            bounds worst-case gate duration when many per-test sandboxes each
            hit a saturated cluster.
        on_retry: Optional callback invoked before each sleep with
            ``(attempt, delay, exc)`` where ``attempt`` is 1-based.

    Returns:
        Whatever ``create`` returns on the first successful attempt.

    Raises:
        The last capacity error once ``retries`` (or ``total_budget``) is
        exhausted, or immediately re-raises any non-capacity error.
    """
    max_retries = max(0, retries)
    attempt = 0
    slept = 0.0
    while True:
        try:
            return create()
        except Exception as exc:  # noqa: BLE001 - decide per-exception whether to retry
            if attempt >= max_retries or not is_capacity_error(exc):
                raise
            attempt += 1
            delay = _backoff_delay(attempt, backoff, backoff_max)
            if total_budget > 0 and slept + delay > total_budget:
                raise
            if on_retry is not None:
                on_retry(attempt, delay, exc)
            time.sleep(delay)
            slept += delay


def _backoff_delay(attempt: int, backoff: float, backoff_max: float) -> float:
    """Full-jitter exponential backoff for a 1-based ``attempt``.

    The ceiling grows as ``backoff * 2 ** (attempt - 1)`` (capped at
    ``backoff_max`` when positive) and the returned delay is a uniform random
    value in ``[0, ceiling]`` to avoid a thundering herd across parallel workers.

    When ``backoff_max <= 0`` the per-delay cap is disabled, but the ceiling is
    still clamped to a large finite value (``_UNCAPPED_BACKOFF_CEILING``) so a
    misconfigured base that overflows to ``inf`` yields a bounded delay rather
    than crashing ``time.sleep`` or collapsing into a tight ``0.0`` loop.
    """
    ceiling = backoff * (2 ** min(attempt - 1, 30))
    if backoff_max > 0:
        ceiling = min(ceiling, backoff_max)
    elif not math.isfinite(ceiling) or ceiling > _UNCAPPED_BACKOFF_CEILING:
        # Uncapped: a huge/overflowed ceiling is clamped to a finite bound so
        # the delay stays large-but-safe instead of inf (raises) or 0.0 (tight
        # loop).
        ceiling = _UNCAPPED_BACKOFF_CEILING
    if not math.isfinite(ceiling) or ceiling < 0.0:
        ceiling = 0.0
    return random.uniform(0.0, ceiling)

