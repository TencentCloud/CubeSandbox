# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

import pytest

from framework.capabilities import FILESYSTEM_EXTENDED

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.filesystem,
    pytest.mark.p1,
    pytest.mark.requires_capability(FILESYSTEM_EXTENDED),
]


def test_write_files_writes_every_entry(sdk_sandbox):
    root = f"/tmp/sdk-compat-write-files-{sdk_sandbox.sandbox_id}"
    files = [
        (f"{root}/alpha.txt", "alpha"),
        (f"{root}/beta.txt", b"beta"),
    ]
    sdk_sandbox.make_dir(root)

    try:
        written = sdk_sandbox.write_files(files)

        assert written == len(files)
        assert sdk_sandbox.read_file(files[0][0]) == "alpha"
        assert sdk_sandbox.read_file(files[1][0]) == "beta"
    finally:
        for path, _ in files:
            if sdk_sandbox.file_exists(path):
                sdk_sandbox.remove_file(path)
        if sdk_sandbox.file_exists(root):
            sdk_sandbox.remove_file(root)


def test_watch_dir_reports_create_write_and_remove(sdk_sandbox, sdk_e2e_config):
    root = f"/tmp/sdk-compat-watch-dir-{sdk_sandbox.sandbox_id}"
    path = f"{root}/watched.txt"
    sdk_sandbox.make_dir(root)

    def mutate() -> None:
        sdk_sandbox.write_file(path, "created")
        time.sleep(0.2)
        sdk_sandbox.write_file(path, "updated")
        time.sleep(0.2)
        sdk_sandbox.remove_file(path)

    def received_target_events(events: list[dict[str, str]]) -> bool:
        target_types = {
            event["type"]
            for event in events
            if event["name"].endswith("watched.txt")
        }
        return {"create", "write", "remove"} <= target_types

    try:
        events = sdk_sandbox.watch_dir_events(
            root,
            mutate,
            timeout=sdk_e2e_config.default_timeout,
            until=received_target_events,
        )
    finally:
        if sdk_sandbox.file_exists(path):
            sdk_sandbox.remove_file(path)
        if sdk_sandbox.file_exists(root):
            sdk_sandbox.remove_file(root)

    target_events = [
        event for event in events if event["name"].endswith("watched.txt")
    ]
    event_types = {event["type"] for event in target_events}
    assert {"create", "write", "remove"} <= event_types, target_events
