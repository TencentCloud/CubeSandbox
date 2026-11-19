# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Exercise snapshot-chain test cleanup without a live sandbox."""

from __future__ import annotations

import copy
from types import SimpleNamespace

import pytest

from cases.lifecycle import test_filesystem_only_snapshot as filesystem_only
from cases.lifecycle import test_snapshot_delete_reference as lifecycle
from framework import cleanup
from framework.models import CodeResult

pytestmark = pytest.mark.framework


@pytest.mark.parametrize(
    ("create_options", "expected"),
    [
        (None, {"template": "snap-1", "timeout": 600}),
        (
            {"timeout": 321, "restartPolicy": "OnFailure"},
            {"template": "snap-1", "timeout": 321, "restartPolicy": "OnFailure"},
        ),
    ],
)
def test_filesystem_snapshot_restore_uses_long_timeout(monkeypatch, create_options, expected):
    captured = None
    sandbox = object()

    def create(*args, **kwargs):
        nonlocal captured
        captured = kwargs["create_options"]
        return sandbox

    monkeypatch.setattr(filesystem_only, "create_adapter_with_capacity_retry", create)
    monkeypatch.setattr(filesystem_only, "wait_until_data_plane_ready", lambda *args, **kwargs: None)

    result = filesystem_only._create_from_snapshot(
        "cubesandbox",
        SimpleNamespace(default_timeout=1, command_timeout=1),
        "snap-1",
        role="child",
        create_options=create_options,
    )

    assert result is sandbox
    assert captured == expected


@pytest.mark.parametrize("delete_before_capture", [False, True])
@pytest.mark.parametrize("restore_failure", [False, True])
def test_snapshot_chain_releases_each_sandbox_once(
    monkeypatch, delete_before_capture, restore_failure
):
    snapshots = {}
    sandboxes = []

    class Sandbox:
        backend = "cubesandbox"

        def __init__(self):
            self.sandbox_id = str(len(sandboxes))
            self.files = {}
            self.namespace = {}
            self.killed = False
            self.kill_count = 0
            sandboxes.append(self)

        def write_file(self, path, value):
            self.files[path] = value

        def read_file(self, path):
            return self.files[path]

        def run_code(self, code, **kwargs):
            exec(code, self.namespace)
            return CodeResult(text="")

        def create_snapshot(self):
            snapshot_id = f"snap-{len(snapshots)}"
            snapshots[snapshot_id] = copy.deepcopy((self.files, self.namespace))
            return snapshot_id

        def delete_snapshot(self, snapshot_id):
            snapshots.pop(snapshot_id, None)
            return False

        def info(self):
            if self.killed:
                raise RuntimeError("sandbox not found")
            return SimpleNamespace(state="running")

        def kill(self):
            self.kill_count += 1
            self.killed = True

        def close(self):
            pass

    def restore(*args, create_options, **kwargs):
        if restore_failure:
            raise RuntimeError("restore failed")
        sandbox = Sandbox()
        sandbox.files, sandbox.namespace = copy.deepcopy(snapshots[create_options["template"]])
        return sandbox

    monkeypatch.setattr(lifecycle, "create_adapter_with_capacity_retry", restore)
    monkeypatch.setattr(cleanup, "_rest_delete", lambda *args: None)
    source = Sandbox()
    config = SimpleNamespace(run_code_timeout=1, default_timeout=1)
    if restore_failure:
        with pytest.raises(RuntimeError, match="restore failed"):
            lifecycle.test_later_snapshot_survives_physical_base_cleanup(
                source, "cubesandbox", config, delete_before_capture
            )
    else:
        lifecycle.test_later_snapshot_survives_physical_base_cleanup(
            source, "cubesandbox", config, delete_before_capture
        )
    assert all(sandbox.kill_count == 1 for sandbox in sandboxes)
    assert not snapshots
