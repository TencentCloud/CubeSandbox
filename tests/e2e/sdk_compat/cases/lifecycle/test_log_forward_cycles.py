# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""E2E lifecycle smoke test for repeated pause/resume.

Companion to commit "shim: serialize log-forward start/stop across Container
clones" (CubeShim/shim/src/container/mod.rs). Pause tears down each container's
log-forward task (``disconnect_agent`` -> ``unset_client`` -> ``stop_log_forward``)
and resume restarts it (``set_client`` -> ``start_log_forward``); the fix shares
one mutexed slot and serializes start/stop with a semaphore so exactly one
caller drains the task to completion instead of falling back to ``abort()``.

Scope note: these SDK-level checks do NOT directly observe the skipped-IO-drain
bug. ``run_command`` streams through envd's process API over HTTP and
``read_file``/``write_file`` hit the sandbox filesystem — neither path traverses
the shim's init log-forward vsock task that the fix changes, so the assertions
here also pass against the pre-fix code. The actual regression guards for the
drain invariant are the Rust unit tests in ``container/mod.rs``
(``log_forward_tests``). What these tests add is a black-box guarantee that
repeated pause/resume cycles stay healthy end-to-end: a wedged drain/resume
would trip ``wait_until_running`` and a corrupted restart would diverge the
command output or marker file.
"""

from __future__ import annotations

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS, PAUSE_RESUME
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
    pytest.mark.requires_capability(COMMANDS),
]

_CYCLES = 4
# 16KB comfortably exceeds the 4096-byte read frame used by the exec-output
# relay, so the payload spans several frames rather than a single read.
_OUTPUT_SIZE = 16384


def test_repeated_pause_resume_keeps_commands_working(sdk_sandbox, sdk_e2e_config):
    """Each pause/resume drives stop/start_log_forward; the cycle must stay healthy."""
    marker_path = "/tmp/sdk-compat-log-forward-cycles.txt"
    sdk_sandbox.write_file(marker_path, "cycle-0")

    # Pause through the handle returned by the previous resume rather than reusing
    # the original pre-pause handle across cycles: a resume may invalidate the old
    # handle's connection, which would fail later cycles for reasons unrelated to
    # the shim change. The fixture owns `sdk_sandbox`; only the intermediate
    # resumed handles are closed here.
    current = sdk_sandbox
    try:
        for cycle in range(_CYCLES):
            current.pause(timeout=sdk_e2e_config.default_timeout)
            assert wait_until_paused(
                current, timeout=sdk_e2e_config.default_timeout
            ) == "paused", f"cycle {cycle}: sandbox did not reach paused"

            resumed = current.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
            if current is not sdk_sandbox:
                current.close()
            current = resumed

            assert wait_until_running(
                current, timeout=sdk_e2e_config.default_timeout
            ) == "running", f"cycle {cycle}: sandbox did not resume to running"

            # Control-plane `running` can precede CubeProxy/envd readiness, so
            # wait for the data plane before hitting read_file/run_command.
            wait_until_data_plane_ready(
                current,
                timeout=sdk_e2e_config.default_timeout,
                command_timeout=sdk_e2e_config.command_timeout,
            )

            # File state must survive every cycle (drain/restart must not corrupt).
            assert current.read_file(marker_path) == f"cycle-{cycle}", (
                f"cycle {cycle}: marker file content diverged"
            )

            result = current.run_command(
                f"printf 'resumed-{cycle}'",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_command_ok(result)
            assert result.stdout == f"resumed-{cycle}", (
                f"cycle {cycle}: command output diverged: {result.stdout!r}"
            )

            current.write_file(marker_path, f"cycle-{cycle + 1}")
    finally:
        if current is not sdk_sandbox:
            current.close()


def test_pause_resume_preserves_large_command_output(
    sdk_sandbox, sdk_backend, sdk_e2e_config
):
    """A large-output command after resume must arrive intact.

    Complements ``test_repeated_pause_resume_keeps_commands_working`` by checking
    that a large exec-output payload survives a resume without truncation or
    corruption. Like that test, this exercises the envd exec-output stream rather
    than the shim's init log-forward vsock task directly; the drain invariant
    itself is guarded by the Rust unit tests. Kept as a large-payload lifecycle
    smoke check.
    """
    # The 16KB payload spans several 4096-byte read frames (the len used by the
    # envd exec-output relay). Another backend's SDK may cap or line-wrap
    # run_command stdout below 16KB, which would fail the exact-equality assert
    # for reasons unrelated to this change.
    if sdk_backend != "cubesandbox":
        pytest.skip(
            f"large-output drain regression is cube-shim specific, got {sdk_backend!r}"
        )
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        assert wait_until_running(
            resumed, timeout=sdk_e2e_config.default_timeout
        ) == "running", "sandbox did not resume to running"

        # Control-plane `running` can precede CubeProxy/envd readiness, so wait
        # for the data plane before running the command.
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )

        payload = "z" * _OUTPUT_SIZE
        result = resumed.run_command(
            f"printf '%s' '{payload}'",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert len(result.stdout) == _OUTPUT_SIZE
        assert result.stdout == payload
    finally:
        resumed.close()
