# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

from framework.config import SdkE2EConfig
from framework.envd_performance import measure, summarize
from framework.preflight import run_preflight

pytestmark = pytest.mark.framework


def test_creating_template_preserves_api_and_sdk_preflight(monkeypatch):
    from framework import preflight
    api = Mock()
    api.health.return_value = {"status": "ok"}
    monkeypatch.setattr(preflight, "ApiClient", lambda config: api)
    dependencies = Mock()
    monkeypatch.setattr(preflight, "_check_backend_dependencies", dependencies)
    monkeypatch.delenv("CUBE_TEMPLATE_ID", raising=False)
    cfg = SdkE2EConfig.from_env()
    reporter = Mock()
    run_preflight(cfg, reporter, require_default_template=False)
    dependencies.assert_called_once()
    api.health.assert_called_once()
    api.get_template.assert_not_called()
    api.close.assert_called_once()
    with pytest.raises(RuntimeError, match="template ID is required"):
        run_preflight(cfg, reporter)
    api.health.side_effect = RuntimeError("offline")
    monkeypatch.setattr(preflight, "_PREFLIGHT_READ_BACKOFF", 0)
    monkeypatch.setattr(preflight, "_retry_transient", lambda fn: fn())
    with pytest.raises(RuntimeError, match="offline"):
        run_preflight(cfg, reporter, require_default_template=False)


def test_explicit_template_still_checked_without_default(monkeypatch):
    from framework import preflight
    api = Mock()
    api.health.return_value = {"status": "ok"}
    api.get_template.return_value = {"status": "FAILED"}
    monkeypatch.setattr(preflight, "ApiClient", lambda config: api)
    monkeypatch.setattr(preflight, "_check_backend_dependencies", lambda *args: None)
    with pytest.raises(RuntimeError, match="not ready"):
        run_preflight(SdkE2EConfig.from_env(), Mock(), template_ids={"tpl-selected"}, require_default_template=False)
    api.get_template.assert_called_once_with("tpl-selected")


def test_collection_routes_new_template_and_performance(monkeypatch):
    import conftest
    def item(*markers):
        return SimpleNamespace(get_closest_marker=lambda name: object() if name in markers else None)
    config = Mock()
    config.option = SimpleNamespace(numprocesses=0)
    config.getoption.side_effect = lambda name, *args: name == "--run-e2e"
    monkeypatch.setenv("SDK_E2E_VOLUME_PLUGIN", "true")
    creation, perf, ordinary = item("creates_template"), item("envd_performance"), item()
    items = [creation, perf]
    conftest.pytest_collection_modifyitems(config, items)
    assert items == [creation]
    assert not config._sdk_e2e_default_template_needed
    conftest.pytest_collection_modifyitems(config, [creation, ordinary])
    assert config._sdk_e2e_default_template_needed
    config.getoption.side_effect = lambda name, *args: name in {"--run-e2e", "--run-envd-performance"}
    config.option.numprocesses = 2
    with pytest.raises(pytest.UsageError, match="serial"):
        conftest.pytest_collection_modifyitems(config, [perf])


def test_measure_preserves_failure_and_excludes_verification(monkeypatch):
    from framework import envd_performance
    clock = iter([0, 2_000_000, 9_000_000, 12_000_000])
    monkeypatch.setattr(envd_performance.time, "perf_counter_ns", lambda: next(clock))
    samples = []
    def fail(result):
        raise AssertionError("wrong bytes")
    measure(samples, lambda: "bad", fail, provider="go")
    def broken():
        raise TimeoutError("deadline")
    measure(samples, broken, fail, provider="rust")
    assert [s["latency_ms"] for s in samples] == [2, 3]
    assert [s["outcome"] for s in samples] == ["error", "error"]
    assert "wrong bytes" in samples[0]["error"]
    assert "deadline" in samples[1]["error"]


def test_summary_counts_errors_and_reports_slower_rust():
    samples = [dict(provider=p, workload="4194304_read", pair=1, phase="measured", outcome="passed", latency_ms=n)
               for p, n in (("go", 10), ("go", 20), ("rust", 20), ("rust", 40))]
    samples.append(dict(provider="rust", workload="4194304_read", pair=1, phase="measured", outcome="error", latency_ms=900))
    go, rust = summarize(samples)
    assert (go["median_ms"], go["p95_ms"]) == (15, 20)
    assert (rust["samples"], rust["successes"], rust["errors"]) == (3, 2, 1)
    assert rust["latency_reduction_percent"] == -100
    assert rust["throughput_gain_percent"] == -50


def test_template_build_error_does_not_wait_for_missing_catalog(monkeypatch):
    from framework import templates
    monkeypatch.setattr(templates.Template, "get_build_status", Mock(return_value=SimpleNamespace(status="error", error_message="guest cgroup v1", message="")))
    get = Mock()
    monkeypatch.setattr(templates.Template, "get", get)
    with pytest.raises(pytest.fail.Exception, match="guest cgroup v1"):
        templates.wait_for_ready("tpl-new", None, timeout=1, build_id="build-new")
    get.assert_not_called()


def test_cleanup_accepts_already_absent_template(monkeypatch):
    from cubesandbox._exceptions import TemplateNotFoundError
    from framework import templates
    monkeypatch.setattr(templates.Template, "delete", Mock(side_effect=TemplateNotFoundError("absent", 404)))
    templates.delete_with_retry("tpl-failed", None, timeout=1)
