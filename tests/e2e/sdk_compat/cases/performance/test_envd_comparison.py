# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""A bounded SDK end-to-end comparison, never a CI speed threshold."""
from __future__ import annotations

import time

import pytest

from framework.envd_performance import measure, summarize
from framework.envd_acceptance import (
    command_ok, health_identity, required_env, require_cube_sdk, sandbox,
)

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.envd_performance]


def verify_command(result):
    assert (result.stdout, result.stderr, result.exit_code) == ("envd-perf", "", 0), result


def verify_content(result, content):
    assert result == content, "full SDK text read differed from payload"


def rss(adapter, pid, timeout):
    try:
        output = command_ok(adapter, f"awk '/^VmRSS:/ {{print $2}}' /proc/{pid}/status", timeout=timeout).strip()
        return {"rss_kib": int(output)}
    except Exception as exc:
        return {"rss_kib": None, "error": str(exc)}


def test_go_rust_sdk_comparison(sdk_e2e_config, sdk_e2e_reporter):
    from contextlib import ExitStack

    require_cube_sdk(sdk_e2e_config)
    config, reporter = sdk_e2e_config, sdk_e2e_reporter
    templates = {p: required_env(f"SDK_ENVD_{p.upper()}_TEMPLATE_ID") for p in ("go", "rust")}
    commits = {p: required_env(f"SDK_ENVD_{p.upper()}_COMMIT") for p in ("go", "rust")}
    payloads = {size: b"x" * size for size in (4096, 4194304)}
    texts = {size: payload.decode("ascii") for size, payload in payloads.items()}
    samples = []
    reporter.record("envd_performance_config", templates=templates, commits=commits,
                    concurrency=1, pairs=3, warmups=5, measured_per_pair=10,
                    timer="perf_counter_ns", quantile="nearest rank: ceil(0.95*n)",
                    read_cache="warm", user="SDK default root", settling_seconds=1,
                    sdk_retries="SDK defaults; no operation retry added; timings include SDK internals",
                    attribution="SDK/client/proxy/network/guest combined; placement and host load require external observation")
    try:
        for pair in (1, 2, 3):
            order = ("go", "rust") if pair % 2 else ("rust", "go")
            reporter.record("envd_performance_pair", pair=pair, order=order)
            with ExitStack() as stack:
                adapters, pids = {}, {}
                for provider in order:
                    adapter = stack.enter_context(sandbox(config, reporter, templates[provider], provider=provider, pair=pair))
                    adapters[provider] = adapter
                    pids[provider] = health_identity(adapter, config, reporter, provider, commits[provider])
                for provider in order:
                    adapter = adapters[provider]
                    raw = adapter.raw_sandbox
                    time.sleep(1)
                    reporter.record("envd_rss", phase="idle", pair=pair, provider=provider,
                                    **rss(adapter, pids[provider], config.command_timeout))
                    operations = [("command", lambda: raw.commands.run("printf 'envd-perf'", timeout=config.command_timeout), verify_command)]
                    for size, payload in payloads.items():
                        path = f"/tmp/envd-perf-{size}"
                        operations.extend([
                            (f"{size}_write", lambda path=path, payload=payload: raw.files.write(path, payload), lambda result: None),
                            (f"{size}_read", lambda path=path: raw.files.read(path), lambda result, size=size: verify_content(result, texts[size])),
                        ])
                    for workload, operation, verify in operations:
                        for attempt in range(16):
                            phase = "first" if attempt == 0 else "warmup" if attempt <= 5 else "measured"
                            # File write correctness uses a full SDK read outside its
                            # timer; no unchecked write attempt counts as a success.
                            if workload.endswith("_write"):
                                size = int(workload.split("_")[0])
                                check = lambda result, size=size: verify_content(raw.files.read(f"/tmp/envd-perf-{size}"), texts[size])
                            else:
                                check = verify
                            measure(samples, operation, check, pair=pair, provider=provider,
                                    workload=workload, phase=phase, attempt=attempt)
                    time.sleep(1)
                    reporter.record("envd_rss", phase="post_workload", pair=pair, provider=provider,
                                    **rss(adapter, pids[provider], config.command_timeout))
    finally:
        # Serialize outside every timed interval, including on an incomplete pair.
        for sample in samples:
            reporter.record("envd_performance_sample", **sample)
        reporter.record("envd_performance_summary", rows=summarize(samples),
                        complete=len(samples) == 480 and all(s["outcome"] == "passed" for s in samples))
    assert len(samples) == 480
    assert all(s["outcome"] == "passed" for s in samples), "failed attempts retained in events.jsonl"
