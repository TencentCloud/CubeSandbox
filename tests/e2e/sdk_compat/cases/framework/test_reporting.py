# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pure-logic unit tests for JsonlReporter's report-file naming.

Marked ``framework`` so they run on every gate without a live environment
(see ``conftest.pytest_collection_modifyitems``). The per-worker filename is the
contract downstream log aggregation depends on: a regression that collapses all
xdist workers onto one path is exactly the interleaving corruption this guards.
"""

from __future__ import annotations

import pytest

from framework.reporting import JsonlReporter

pytestmark = pytest.mark.framework


def test_serial_run_uses_plain_events_file(tmp_path, monkeypatch):
    monkeypatch.delenv("PYTEST_XDIST_WORKER", raising=False)
    reporter = JsonlReporter(tmp_path)
    try:
        assert reporter._path.name == "events.jsonl"
    finally:
        reporter.close()


def test_xdist_worker_gets_own_file(tmp_path, monkeypatch):
    monkeypatch.setenv("PYTEST_XDIST_WORKER", "gw3")
    reporter = JsonlReporter(tmp_path)
    try:
        assert reporter._path.name == "events-gw3.jsonl"
    finally:
        reporter.close()
