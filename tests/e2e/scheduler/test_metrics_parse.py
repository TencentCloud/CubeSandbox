# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import pytest

from metrics import parse_attempts, parse_plugin_counts, plugin_deltas, sole_increased_profile, success_deltas

pytestmark = pytest.mark.framework

_SAMPLE = """\
# HELP scheduler_attempts_total Total scheduler Select attempts.
scheduler_attempts_total{profile="default",reason="none",result="success"} 4
scheduler_attempts_total{profile="default",reason="no_node",result="error"} 1
scheduler_attempts_total{profile="burst_balance",reason="none",result="success"} 2
scheduler_duration_seconds_bucket{profile="default",le="0.001"} 3
scheduler_plugin_duration_seconds_count{kind="filter",plugin="deny_all",profile="cel_deny"} 2
"""


def test_parse_attempts_keeps_profile_and_result() -> None:
    got = parse_attempts(_SAMPLE)
    assert got == {
        ("default", "success"): 4.0,
        ("default", "error"): 1.0,
        ("burst_balance", "success"): 2.0,
    }


def test_success_deltas_treats_missing_series_as_zero() -> None:
    before = {("default", "success"): 4.0}
    after = {
        ("default", "success"): 4.0,
        ("burst_balance", "success"): 1.0,
    }
    assert success_deltas(before, after) == {"burst_balance": 1.0}


def test_plugin_deltas_ignore_other_plugins() -> None:
    before = parse_plugin_counts(_SAMPLE)
    after = parse_plugin_counts(
        _SAMPLE.replace(
            'scheduler_plugin_duration_seconds_count{kind="filter",plugin="deny_all",profile="cel_deny"} 2',
            'scheduler_plugin_duration_seconds_count{kind="filter",plugin="deny_all",profile="cel_deny"} 3\n'
            'scheduler_plugin_duration_seconds_count{kind="score",plugin="other",profile="default"} 1',
        )
    )
    assert plugin_deltas(before, after, plugin="deny_all", kind="filter") == {"cel_deny": 1.0}


def test_sole_increased_profile_rejects_zero_or_many() -> None:
    assert sole_increased_profile({"default": 1.0}) == "default"
    with pytest.raises(AssertionError, match="exactly one"):
        sole_increased_profile({})
    with pytest.raises(AssertionError, match="exactly one"):
        sole_increased_profile({"default": 1.0, "burst_balance": 1.0})
