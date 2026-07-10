# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
import uuid
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
SDK_COMPAT_ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(SDK_COMPAT_ROOT))

from adapters import create_adapter  # noqa: E402
from framework.capabilities import (  # noqa: E402
    CUBESANDBOX_CAPABILITIES,
    E2B_CAPABILITIES,
)
from framework.cleanup import safe_kill  # noqa: E402
from framework.config import SdkE2EConfig  # noqa: E402
from framework.preflight import run_preflight  # noqa: E402
from framework.reporting import JsonlReporter  # noqa: E402


def pytest_addoption(parser: pytest.Parser) -> None:
    group = parser.getgroup("sdk compat e2e")
    group.addoption(
        "--run-e2e",
        action="store_true",
        default=False,
        help="run tests that hit a live CubeAPI/CubeProxy environment",
    )
    group.addoption(
        "--sdk-e2e-backends",
        default=None,
        help="comma-separated backends to run; defaults to cubesandbox",
    )
    group.addoption(
        "--cube-api-url",
        default=None,
        help="CubeAPI URL; defaults to CUBE_API_URL or http://127.0.0.1:3000",
    )
    group.addoption(
        "--cube-template-id",
        default=None,
        help="template ID for SDK E2E; defaults to CUBE_TEMPLATE_ID",
    )


def pytest_configure(config: pytest.Config) -> None:
    for marker in (
        "sdk_compat: SDK compatibility E2E tests",
        "requires_capability(name): current SDK backend must support this capability",
        "requires_internet: test requires public internet access from the sandbox",
        "requires_cubeproxy: test requires CubeProxy routing to the sandbox",
    ):
        config.addinivalue_line("markers", marker)


def pytest_generate_tests(metafunc: pytest.Metafunc) -> None:
    if "sdk_backend" not in metafunc.fixturenames:
        return
    cfg = _config_from_pytest(metafunc.config)
    metafunc.parametrize("sdk_backend", cfg.backends, ids=list(cfg.backends))


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    if config.getoption("--run-e2e"):
        return
    skip = pytest.mark.skip(reason="live SDK E2E disabled; pass --run-e2e to run")
    for item in items:
        item.add_marker(skip)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item: pytest.Item, call: pytest.CallInfo):
    outcome = yield
    report = outcome.get_result()
    if report.when not in {"setup", "call", "teardown"}:
        return
    if report.when == "setup" and report.passed:
        return

    reporter = item.funcargs.get("sdk_e2e_reporter")
    if reporter is None:
        return

    adapter = item.funcargs.get("sdk_sandbox")
    sandbox_id = getattr(adapter, "sandbox_id", None)
    backend = item.funcargs.get("sdk_backend") or getattr(adapter, "backend", None)
    payload = {
        "nodeid": item.nodeid,
        "backend": backend,
        "sandbox_id": sandbox_id,
        "phase": report.when,
        "outcome": report.outcome,
        "duration": report.duration,
    }
    if report.failed:
        payload["error"] = report.longreprtext
        if adapter is not None:
            try:
                payload["sandbox_info"] = adapter.info().raw
            except Exception as exc:  # noqa: BLE001 - diagnostics must not hide the failure
                payload["sandbox_info_error"] = str(exc)
    elif report.skipped:
        payload["reason"] = str(report.longrepr)

    reporter.record_test_result(**payload)


@pytest.fixture(scope="session")
def sdk_e2e_config(pytestconfig: pytest.Config) -> SdkE2EConfig:
    cfg = _config_from_pytest(pytestconfig)
    if cfg.cube_python_sdk_path:
        sys.path.insert(0, cfg.cube_python_sdk_path)
    else:
        sys.path.insert(0, str(ROOT / "sdk" / "python"))
    for key, value in cfg.env().items():
        os.environ.setdefault(key, value)
    return cfg


@pytest.fixture(scope="session")
def sdk_e2e_reporter(sdk_e2e_config: SdkE2EConfig) -> JsonlReporter:
    return JsonlReporter(sdk_e2e_config.report_dir)


@pytest.fixture(scope="session", autouse=True)
def sdk_e2e_preflight(pytestconfig: pytest.Config, sdk_e2e_config: SdkE2EConfig, sdk_e2e_reporter: JsonlReporter):
    if not pytestconfig.getoption("--run-e2e"):
        return
    try:
        run_preflight(sdk_e2e_config, sdk_e2e_reporter)
    except RuntimeError as exc:
        pytest.exit(str(exc), returncode=2)


@pytest.fixture()
def sdk_sandbox(
    request: pytest.FixtureRequest,
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
    sdk_e2e_reporter: JsonlReporter,
):
    for marker in request.node.iter_markers("requires_capability"):
        capability = marker.args[0]
        if capability not in _capabilities_for_backend(sdk_backend):
            pytest.skip(f"backend {sdk_backend!r} does not support capability {capability!r}")

    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for SDK E2E")

    metadata = {
        "test_suite": "sdk_compat",
        "test_backend": sdk_backend,
        "test_nodeid": request.node.nodeid,
        "test_run_id": uuid.uuid4().hex,
    }
    request.node._sdk_e2e_backend = sdk_backend
    try:
        adapter = create_adapter(sdk_backend, sdk_e2e_config, metadata=metadata)
    except ImportError as exc:
        pytest.skip(str(exc))
    request.node._sdk_e2e_sandbox_id = adapter.sandbox_id

    sdk_e2e_reporter.record(
        "sandbox_created",
        backend=sdk_backend,
        sandbox_id=adapter.sandbox_id,
        nodeid=request.node.nodeid,
    )
    try:
        yield adapter
    finally:
        if sdk_e2e_config.keep_sandbox_on_failure:
            sdk_e2e_reporter.record(
                "sandbox_kept",
                backend=sdk_backend,
                sandbox_id=adapter.sandbox_id,
                nodeid=request.node.nodeid,
            )
        else:
            errors = safe_kill(adapter, sdk_e2e_config)
            sdk_e2e_reporter.record(
                "sandbox_cleanup",
                backend=sdk_backend,
                sandbox_id=adapter.sandbox_id,
                nodeid=request.node.nodeid,
                errors=errors,
            )


def _config_from_pytest(config: pytest.Config) -> SdkE2EConfig:
    return SdkE2EConfig.from_env(
        backends=config.getoption("--sdk-e2e-backends"),
        cube_api_url=config.getoption("--cube-api-url"),
        cube_template_id=config.getoption("--cube-template-id"),
    )


def _capabilities_for_backend(backend: str) -> frozenset[str]:
    if backend == "cubesandbox":
        return CUBESANDBOX_CAPABILITIES
    if backend == "e2b":
        return E2B_CAPABILITIES
    return frozenset()
