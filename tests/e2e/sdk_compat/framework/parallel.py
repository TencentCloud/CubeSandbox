# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Resolve the pytest-xdist worker count for the live SDK E2E suite.

Kept as a pure helper so the branching (opt-in serial fallback, disable tokens,
``auto``/``logical`` passthrough, numeric parsing) can be unit-tested without
pytest-xdist or a live environment.
"""

from __future__ import annotations

import os

# Upper bound for the *fallback* worker count when the authoritative
# ``PYTEST_XDIST_WORKER_COUNT`` is missing. The fallback multiplies timeout and
# capacity-retry budgets, so an uncapped ``os.cpu_count()`` on a many-core box
# would inflate a 300s READY budget into hours. Cap it to a sane ceiling: still
# large enough to keep timeout and retry budgets generous, but not absurd. The
# authoritative count (when present) is trusted as-is because the
# operator chose it explicitly via ``-n``/``SDK_E2E_WORKERS``.
_MAX_FALLBACK_WORKERS = 8

# Tokens that force a serial run (leave ``numprocesses`` unset).
_DISABLE_TOKENS = {"0", "1", "no", "off"}
# Values xdist expects as strings; a fixed count must be an int because xdist
# builds ``["popen"] * numprocesses`` from it.
_PASSTHROUGH_TOKENS = {"auto", "logical"}


def resolve_worker_count(env_value: str | None) -> int | str | None:
    """Return the xdist ``numprocesses`` value, or ``None`` to stay serial.

    ``env_value`` is the raw ``SDK_E2E_WORKERS`` string (or ``None``); matching
    is case-insensitive. Raises ``ValueError`` on a malformed or non-positive
    count so the caller can surface a clean message.

    Parallelism is strictly opt-in: an unset value keeps the run serial so the
    default ``pytest --run-e2e`` does not overload the co-located control plane.
    Pass ``auto``/``logical`` (resolved by xdist) or an explicit count to fan out.
    """
    value = (env_value or "").strip().lower()
    if not value or value in _DISABLE_TOKENS:
        return None
    if value in _PASSTHROUGH_TOKENS:
        return value
    try:
        count = int(value)
    except ValueError:
        raise ValueError(
            "SDK_E2E_WORKERS must be a positive integer, 'auto', 'logical', "
            f"or a disable token (0/1/no/off); got {env_value!r}"
        ) from None
    if count < 1:
        raise ValueError(f"SDK_E2E_WORKERS must be >= 1; got {env_value!r}")
    return count


def current_worker_count() -> int:
    """Best-effort number of xdist workers in the running session (``1`` serial).

    Reads the environment xdist injects into each worker process:
    ``PYTEST_XDIST_WORKER_COUNT`` is authoritative and set by ``pytest-xdist``
    (present since well before the pinned ``>=3.0``). It reflects the operator's
    chosen ``-n``, so it is trusted as-is with no cap. If it is somehow absent we
    cannot know the true count -- deriving ``gwN + 1`` from ``PYTEST_XDIST_WORKER``
    would return a *different* value on every worker (``gw0``->1, ``gw1``->2, ...)
    and desync ``scale_timeout_for_xdist`` and
    ``_scale_capacity_retries_for_xdist`` across workers. Fall back to a value
    every worker agrees on: ``os.cpu_count()`` (a host constant) capped at
    ``_MAX_FALLBACK_WORKERS`` -- biased high enough to keep timeout/retry budgets
    generous, but bounded so they are not inflated into hours on a many-core box.
    If we are not under xdist at all, the
    run is serial, so return ``1``.
    """
    count = os.environ.get("PYTEST_XDIST_WORKER_COUNT")
    if count:
        try:
            parsed = int(count)
            if parsed >= 1:
                return parsed
        except ValueError:
            pass
    if os.environ.get("PYTEST_XDIST_WORKER"):
        return min(os.cpu_count() or 1, _MAX_FALLBACK_WORKERS)
    return 1


def wants_parallel(env_value: str | None) -> bool:
    """True when ``SDK_E2E_WORKERS`` asks for parallelism (not unset/disabled).

    Lets the conftest distinguish "env var unset/disabled" (serial by choice)
    from "env var asked for workers but xdist is unavailable" (a misconfig worth
    warning about), without duplicating the token/parsing rules. Malformed values
    count as a request so the mismatch is still surfaced rather than swallowed.
    """
    try:
        return resolve_worker_count(env_value) is not None
    except ValueError:
        return True


def apply_worker_count(config) -> int | str | None:
    """Resolve ``SDK_E2E_WORKERS`` into xdist's ``numprocesses`` on ``config``.

    Returns the value written (an int, or ``auto``/``logical``) or ``None`` when
    the run stays serial. Kept here (rather than inline in the conftest hook) so
    it can be unit-tested against a stub config/plugin-manager: it only reads
    ``config.getoption('--run-e2e')``, ``config.pluginmanager.hasplugin('xdist')``
    and ``config.option.numprocesses``, and on opt-in writes ``numprocesses``.
    Raises ``ValueError`` (from ``resolve_worker_count``) on a malformed value so
    the caller can surface a clean usage error.
    """
    # xdist re-runs the ``pytest_cmdline_main`` hook inside every worker process,
    # where ``numprocesses`` is always ``None`` (the worker argv carries no
    # ``-n``). Only the controller spawns workers, so re-deriving the count and
    # writing ``numprocesses`` back into a worker's own config is both useless and
    # actively misleading (it would set ``_sdk_e2e_expected_parallel`` and make
    # ``_verify_xdist_activated`` emit a spurious serial warning from each worker).
    # A worker inherits its parallelism from the controller; stay out of its way.
    if os.environ.get("PYTEST_XDIST_WORKER"):
        return None
    if not config.getoption("--run-e2e"):
        return None
    # ``-p no:xdist`` unregisters the plugin, so honoring it falls out here.
    if not config.pluginmanager.hasplugin("xdist"):
        return None
    # Respect an explicit ``-n``/``--numprocesses`` (including ``-n0``, which
    # xdist stores as int 0 to force serial); ``None`` means the flag was unset.
    if getattr(config.option, "numprocesses", None) is not None:
        return None
    workers = resolve_worker_count(os.environ.get("SDK_E2E_WORKERS"))
    if workers is not None:
        config.option.numprocesses = workers
    return workers


def scale_timeout_for_xdist(base: int) -> int:
    """Widen a per-item wait ``base`` by the xdist worker count.

    Live template builds serialize on CubeMaster's per-artifactID build lock and
    contend with every worker's sandbox creates, so a wait sized for a serial run
    can expire on a healthy-but-loaded control plane once N workers submit at
    once. Multiply by the worker count (``1`` serial → unchanged).
    """
    return base * max(current_worker_count(), 1)
