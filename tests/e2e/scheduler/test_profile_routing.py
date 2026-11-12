# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Live checks that ordinary creates stay on the default profile.

The one-click scheduler config routes only workload=cel_deny. Unlabeled
creates and the factory workload labels stay on profile "default", which
runs the mandatory guards and no CEL plugin.
"""

from __future__ import annotations

import pytest

from metrics import sole_increased_profile

pytestmark = [pytest.mark.e2e, pytest.mark.scheduler]


def _successful_profile(session, metadata: dict[str, str] | None = None) -> tuple[str, dict[str, float]]:
    attempt = session.create(metadata)
    assert attempt.error is None, attempt.error
    profile = sole_increased_profile(attempt.success_deltas)
    assert attempt.success_deltas[profile] >= 1, attempt.success_deltas
    assert attempt.deny_all_deltas == {}, attempt.deny_all_deltas
    return profile, attempt.success_deltas


def test_unlabeled_create_uses_default_profile(routing) -> None:
    session, _observation = routing
    profile, deltas = _successful_profile(session)
    assert profile == "default", deltas


def test_burst_label_stays_on_default_profile(routing) -> None:
    session, _observation = routing
    profile, deltas = _successful_profile(session, {"workload": "burst_balance"})
    assert profile == "default", deltas


def test_template_reuse_label_stays_on_default_profile(routing) -> None:
    session, _observation = routing
    profile, deltas = _successful_profile(session, {"workload": "template_reuse"})
    assert profile == "default", deltas
