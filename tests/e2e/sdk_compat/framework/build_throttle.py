# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Cross-process throttle for live template builds.

Building a template from an image is the single most control-plane-intensive
operation in the suite: CubeMaster pulls the image, builds an ext4 rootfs
artifact, and distributes it. On a single co-located node these builds do NOT
fan out cleanly — concurrent builds of distinct fingerprints contend on the
shared image pull / build host, and under enough of them a build goroutine can
starve long past any per-test READY budget.

That makes template-building cases *load-sensitive*: at ``-n1`` a build always
completes, but at high ``-n`` several fire at once and some time out — so the
same test yields a different outcome purely because of the worker count. To keep
results identical to ``pytest -n1 --run-e2e`` at any concurrency, serialize the
builds across worker processes with a filesystem lock. The lock is held across
build *and* the wait for READY, so the number of in-flight builds never exceeds
``SDK_E2E_TEMPLATE_BUILD_CONCURRENCY`` (default 1) regardless of ``-n``.

The lock is a POSIX ``fcntl`` advisory lock over a small set of slot files in a
per-UID temp dir. It degrades to a no-op if ``fcntl`` is unavailable (non-POSIX),
or the lock dir cannot be created/trusted, so it never blocks a run it cannot
coordinate.
"""

from __future__ import annotations

import contextlib
import os
import sys
import tempfile
import time
from collections.abc import Iterator
from datetime import datetime, timezone
from pathlib import Path

from framework.parallel import current_worker_count

try:  # POSIX only; the throttle degrades to a no-op without it.
    import fcntl
except ImportError:  # pragma: no cover - non-POSIX fallback
    fcntl = None  # type: ignore[assignment]

# Shared across the workers of a single user's run. Kept out of the report dir so
# it is never collected as an artifact, and namespaced per-UID so a concurrent
# run by another user on a shared host coordinates through its own slot files
# rather than cross-coupling (and cannot squat this user's predictable path).
# ``getuid`` is POSIX-only, matching ``fcntl``; the throttle is a no-op without it.
#
# The namespace is deliberately per-UID and NOT per-run: two concurrent
# ``--run-e2e`` jobs of the *same* user on one host (e.g. parallel self-hosted CI
# jobs) share these slot files and serialize their template builds against each
# other. That is intended -- both jobs contend on the one shared CubeMaster build
# host, so throttling across them is exactly the coordination this lock exists to
# provide; a per-run identifier would let them build in lockstep and reintroduce
# the contention the throttle prevents. The bounded ``_slot_wait_timeout`` still
# caps how long such cross-run waiting can last before degrading to unthrottled.
_UID = getattr(os, "getuid", lambda: "shared")()
_LOCK_DIR = Path(tempfile.gettempdir()) / f"cube-sdk-e2e-template-build.locks-{_UID}"

_DEFAULT_CONCURRENCY = 1
_POLL_INTERVAL = 0.5
# Bound the wait for a slot so one worker whose build never reaches READY cannot
# stall every other template-building worker forever. The holder keeps the slot
# across ``Template.build`` *and* the worker-count-scaled READY wait, so size the
# ceiling generously (per-worker, since up to N-1 peers may build ahead of us),
# but keep it finite. A serial run can wait this full base behind another same-UID
# run because the per-UID namespace intentionally coordinates across runs.
# Overridable via env for slow control planes.
_DEFAULT_SLOT_WAIT_BASE = 1800  # 30 min per potential predecessor build


def _log(message: str) -> None:
    worker = os.environ.get("PYTEST_XDIST_WORKER", "-")
    timestamp = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
    print(
        f"[sdk-e2e][{timestamp}][{worker}] build-throttle {message}",
        file=sys.stderr,
        flush=True,
    )


def _slot_wait_timeout() -> float:
    """Ceiling (seconds) on how long to wait for a build slot before degrading.

    Scales with the worker count: with N workers up to N-1 peers may be building
    ahead of us, each holding the slot across its own scaled READY wait. Env
    ``SDK_E2E_TEMPLATE_BUILD_WAIT`` overrides the per-predecessor base for slow
    control planes; a non-positive value disables the ceiling (wait forever).
    """
    raw = os.environ.get("SDK_E2E_TEMPLATE_BUILD_WAIT", "").strip()
    base = _DEFAULT_SLOT_WAIT_BASE
    if raw:
        try:
            base = int(raw)
        except ValueError:
            base = _DEFAULT_SLOT_WAIT_BASE
    if base <= 0:
        return float("inf")
    return base * max(current_worker_count(), 1)


def _concurrency() -> int:
    raw = os.environ.get("SDK_E2E_TEMPLATE_BUILD_CONCURRENCY", "").strip()
    if not raw:
        return _DEFAULT_CONCURRENCY
    try:
        value = int(raw)
    except ValueError:
        return _DEFAULT_CONCURRENCY
    return value if value >= 1 else _DEFAULT_CONCURRENCY


@contextlib.contextmanager
def template_build_slot(label: str = "") -> Iterator[None]:
    """Hold one of the limited template-build slots for the duration of a build.

    Acquire before ``Template.build`` and keep the context open until the
    template has reached READY (or failed), so concurrent in-flight builds stay
    bounded. A no-op when the lock cannot help: ``fcntl`` is unavailable
    (non-POSIX), or the lock dir cannot be created.

    ``label`` names the build site (e.g. ``alias_rebuild_a``) and is logged on
    slot wait/acquire/release so throttle contention is diagnosable when a
    parallel run stalls on the shared build host.
    """
    slots = _concurrency()
    if fcntl is None:
        yield
        return
    try:
        _LOCK_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
        st = _LOCK_DIR.stat()
        if st.st_uid != os.getuid() or (st.st_mode & 0o077):
            # Foreign-owned or group/other-accessible (possibly squatted on a
            # shared host): do not trust it to coordinate, and do not block.
            yield
            return
    except OSError:
        # Cannot coordinate; do not block the run.
        yield
        return

    timeout = _slot_wait_timeout()
    _log(
        f"waiting for a build slot ({slots} slot(s), timeout={timeout}s) label={label!r}"
    )
    try:
        acquired = _acquire_any_slot(slots, timeout)
    except OSError as exc:
        # A slot path could not be opened/locked -- e.g. ELOOP from a symlink
        # planted at a predictable slot path (O_NOFOLLOW), EACCES, or ENOSPC.
        # The throttle promises never to block a run it cannot coordinate, so
        # degrade to serial (yield through) rather than failing the build with
        # an error unrelated to what the test is exercising.
        _log(
            f"could not acquire a slot ({exc}); proceeding unthrottled label={label!r}"
        )
        yield
        return

    if acquired is None:
        # Waited the full ceiling without winning a slot -- a peer's build is
        # wedged. Proceed unthrottled rather than stalling this worker forever;
        # the OS releases the peer's flock on process death, so this is a
        # last-resort escape, not a silent disable of the throttle.
        _log(
            f"timed out after {timeout}s waiting for a slot; proceeding unthrottled label={label!r}"
        )
        yield
        return

    handle, slot_index = acquired
    _log(f"acquired slot {slot_index} label={label!r}")
    try:
        yield
    finally:
        with contextlib.suppress(Exception):
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
        with contextlib.suppress(Exception):
            handle.close()
        _log(f"released slot {slot_index} label={label!r}")


def _acquire_any_slot(slots: int, timeout: float = float("inf")):
    """Wait until one of ``slots`` lock files can be exclusively locked.

    Returns ``(handle, index)`` on success, or ``None`` if ``timeout`` seconds
    elapse first (an infinite ``timeout`` never gives up). Raises ``OSError`` if
    a slot path cannot be opened/locked at all.
    """
    deadline = time.monotonic() + timeout
    handles = []
    try:
        for index in range(slots):
            path = _LOCK_DIR / f"slot-{index}.lock"
            # O_NOFOLLOW + owner-only perms: never follow a symlink someone may
            # have planted at a predictable slot path on a shared host.
            fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            handles.append((index, os.fdopen(fd, "w")))
        while True:
            for index, handle in handles:
                try:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except OSError:
                    continue
                # Won this slot: keep its handle, close the rest.
                for other_index, other in handles:
                    if other_index != index:
                        with contextlib.suppress(Exception):
                            other.close()
                return handle, index
            if time.monotonic() >= deadline:
                for _index, handle in handles:
                    with contextlib.suppress(Exception):
                        handle.close()
                return None
            time.sleep(_POLL_INTERVAL)
    except BaseException:
        for _index, handle in handles:
            with contextlib.suppress(Exception):
                handle.close()
        raise
