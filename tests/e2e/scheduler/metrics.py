# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Read scheduler_attempts_total from a Prometheus text scrape."""

from __future__ import annotations

import re
from urllib.request import urlopen

_LABEL = re.compile(r'([A-Za-z_][A-Za-z0-9_]*)="([^"]*)"')
_ATTEMPTS = "scheduler_attempts_total"
_PLUGIN_COUNT = "scheduler_plugin_duration_seconds_count"


class Scrape:
    def __init__(
        self,
        attempts: dict[tuple[str, str], float],
        plugin_counts: dict[tuple[str, str, str], float],
    ) -> None:
        self.attempts = attempts
        self.plugin_counts = plugin_counts


def scrape(url: str, timeout: float = 10.0) -> Scrape:
    """Read scheduler attempt counters and per-plugin invocation counts."""
    with urlopen(url, timeout=timeout) as response:
        text = response.read().decode("utf-8", errors="replace")
    return Scrape(parse_attempts(text), parse_plugin_counts(text))


def parse_attempts(text: str) -> dict[tuple[str, str], float]:
    samples: dict[tuple[str, str], float] = {}
    for line in text.splitlines():
        if not line.startswith(_ATTEMPTS + "{"):
            continue
        body, _, raw_value = line.partition("} ")
        if not raw_value:
            continue
        labels = dict(_LABEL.findall(body))
        profile = labels.get("profile")
        result = labels.get("result")
        if profile is None or result is None:
            continue
        samples[(profile, result)] = float(raw_value.split()[0])
    return samples


def parse_plugin_counts(text: str) -> dict[tuple[str, str, str], float]:
    """Return {(profile, plugin, kind): count} for plugin duration samples."""
    samples: dict[tuple[str, str, str], float] = {}
    for line in text.splitlines():
        if not line.startswith(_PLUGIN_COUNT + "{"):
            continue
        body, _, raw_value = line.partition("} ")
        if not raw_value:
            continue
        labels = dict(_LABEL.findall(body))
        profile = labels.get("profile")
        plugin = labels.get("plugin")
        kind = labels.get("kind")
        if profile is None or plugin is None or kind is None:
            continue
        samples[(profile, plugin, kind)] = float(raw_value.split()[0])
    return samples


def plugin_deltas(
    before: dict[tuple[str, str, str], float],
    after: dict[tuple[str, str, str], float],
    *,
    plugin: str,
    kind: str,
) -> dict[str, float]:
    """Profiles whose invocation count for one plugin increased."""
    profiles = {profile for profile, name, stage in set(before) | set(after) if name == plugin and stage == kind}
    deltas: dict[str, float] = {}
    for profile in profiles:
        key = (profile, plugin, kind)
        delta = after.get(key, 0.0) - before.get(key, 0.0)
        if delta > 0:
            deltas[profile] = delta
    return deltas


def result_deltas(
    before: dict[tuple[str, str], float],
    after: dict[tuple[str, str], float],
    result: str,
) -> dict[str, float]:
    """Profiles whose attempt counter for one result increased. Absent series count as 0."""
    profiles = {profile for profile, name in set(before) | set(after) if name == result}
    deltas: dict[str, float] = {}
    for profile in profiles:
        delta = after.get((profile, result), 0.0) - before.get((profile, result), 0.0)
        if delta > 0:
            deltas[profile] = delta
    return deltas


def success_deltas(
    before: dict[tuple[str, str], float],
    after: dict[tuple[str, str], float],
) -> dict[str, float]:
    return result_deltas(before, after, "success")


def sole_increased_profile(deltas: dict[str, float]) -> str:
    if len(deltas) != 1:
        raise AssertionError(
            "expected exactly one profile success counter to increase, "
            f"got {deltas}"
        )
    return next(iter(deltas))
