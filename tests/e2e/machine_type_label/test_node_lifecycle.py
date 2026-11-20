# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Cubelet re-registration must not clobber operator-declared labels.

Kept in its own module: restarting Cubelet briefly makes the node
unschedulable, so these run after the label/restore suites (pytest orders files
alphabetically) instead of racing them.
"""

from __future__ import annotations

import subprocess
import time

import pytest

from conftest import MACHINE_TYPE_KEY, wait_until_schedulable

pytestmark = [pytest.mark.e2e, pytest.mark.machine_type, pytest.mark.node_lifecycle]


def _restart_cubelet_or_skip() -> None:
    started = subprocess.run(
        ["systemctl", "restart", "cube-sandbox-cubelet"],
        capture_output=True,
        text=True,
        check=False,
    )
    if started.returncode != 0:
        pytest.skip(f"cannot restart cubelet: {started.stderr.strip()}")


def test_machine_type_is_not_overwritten_by_cubelet(ops, node_id, clean_labels) -> None:
    """A Cubelet restart re-registers its own label set; machine-type is
    control-plane owned and must survive."""
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    # Restarting is only possible when the suite runs on the node itself.
    if subprocess.run(["systemctl", "is-active", "--quiet", "cube-sandbox-cubelet"]).returncode != 0:
        pytest.skip("cubelet is not managed by systemd on this runner")
    _restart_cubelet_or_skip()
    deadline = time.time() + 60
    while time.time() < deadline:
        labels = ops.labels(node_id)
        if labels.get(MACHINE_TYPE_KEY) == "intel-e2e-k66":
            break
        time.sleep(2)
    assert ops.labels(node_id).get(MACHINE_TYPE_KEY) == "intel-e2e-k66"
    wait_until_schedulable(ops, node_id)


def test_data_plane_labels_survive_cubelet_restart(ops, node_id, sandboxes, clean_labels) -> None:
    """Cubelet owns the data-plane labels; a restart must restore them, and
    the operator's machine-type must stay untouched alongside them."""
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    before = ops.labels(node_id)
    system_before = {
        k: v for k, v in before.items() if k.startswith(("kubernetes.io/", "beta.kubernetes.io/"))
    }
    if not system_before:
        pytest.skip("node reports no data-plane labels")

    _restart_cubelet_or_skip()
    wait_until_schedulable(ops, node_id)

    after = ops.labels(node_id)
    for key, value in system_before.items():
        assert after.get(key) == value, f"cubelet restart lost data-plane label {key}"
    assert after.get(MACHINE_TYPE_KEY) == "intel-e2e-k66"

    _assert_cluster_usable(sandboxes)


# Errors that mean "CubeMaster is still rebuilding its template view after the
# re-registration". Anything else is a real failure and must surface as one.
_TRANSIENT = ("no more resource", "is not ready on any healthy node")


def _assert_cluster_usable(sandboxes, attempts: int = 12, delay: float = 10.0) -> None:
    """Prove the node can host a sandbox again, without hiding real failures.

    Only the two known post-restart windows are retried (CubeMaster rebuilds
    its template-replica view after re-registration, ~25s measured); anything
    else fails immediately.
    """
    from cubesandbox import ApiError

    for attempt in range(attempts):
        try:
            sandbox = sandboxes.create()
            sandbox.kill()
            sandboxes.created.remove(sandbox)
            return
        except ApiError as exc:
            if not any(marker in str(exc) for marker in _TRANSIENT):
                raise
            if attempt + 1 == attempts:
                raise AssertionError(
                    f"cluster still refused sandboxes after {attempts} attempts: {exc}"
                ) from exc
            time.sleep(delay)
