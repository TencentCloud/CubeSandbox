# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Live checks for the opt-in CEL deny profile shipped in the one-click config."""

from __future__ import annotations

import pytest

pytestmark = [pytest.mark.e2e, pytest.mark.scheduler]


def test_unlabeled_create_does_not_run_cel_filter(routing) -> None:
    session, _observation = routing
    attempt = session.create()
    assert attempt.error is None, attempt.error
    assert list(attempt.success_deltas) == ["default"], attempt.success_deltas
    assert attempt.success_deltas["default"] >= 1
    assert attempt.deny_all_deltas == {}, attempt.deny_all_deltas


def test_cel_deny_label_is_rejected_by_expression_filter(routing) -> None:
    session, _observation = routing
    attempt = session.create(metadata={"workload": "cel_deny"})
    assert attempt.error is not None, "cel_deny create succeeded"
    assert attempt.sandbox is None
    assert attempt.error_deltas.get("cel_deny", 0) >= 1, attempt.error_deltas
    assert attempt.deny_all_deltas.get("cel_deny", 0) >= 1, attempt.deny_all_deltas
