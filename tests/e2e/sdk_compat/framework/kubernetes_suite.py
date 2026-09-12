# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Reuse the existing backend-neutral smoke cases in the post-install suite."""

SHARED_CASES = {
    "lifecycle/test_create.py::test_create_returns_usable_sandbox",
    "lifecycle/test_kill.py::test_kill_prevents_reconnect",
    "commands/test_run.py::test_command_stdout_stderr_and_exit_code",
    "filesystem/test_read_write.py::test_file_write_read_roundtrip",
}


def select_post_install_tests(items):
    selected, deselected = [], []
    for item in items:
        nodeid = item.nodeid.split("[", 1)[0]
        include = bool(item.get_closest_marker("k8s_post_install")) or any(
            nodeid.endswith("/" + case) for case in SHARED_CASES
        )
        (selected if include else deselected).append(item)
    return selected, deselected
