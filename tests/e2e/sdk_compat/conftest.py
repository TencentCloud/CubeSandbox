# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
import uuid
from dataclasses import replace
from datetime import datetime, timezone
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
SDK_COMPAT_ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(SDK_COMPAT_ROOT))

from adapters import create_adapter  # noqa: E402
from framework.cleanup import safe_kill  # noqa: E402
from framework.capabilities import CODE_INTERPRETER, capabilities_for_backend  # noqa: E402
from framework.config import SdkE2EConfig  # noqa: E402
from framework.preflight import run_preflight  # noqa: E402
from framework.reporting import JsonlReporter  # noqa: E402
from framework.trace import TraceCollector, reset_current_trace, set_current_trace  # noqa: E402


def _load_dotenv(path: Path) -> None:
    if not path.exists():
        return
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        if line.startswith("export "):
            line = line[len("export ") :].strip()
        key, value = line.split("=", 1)
        key = key.strip()
        value = _strip_env_value(value.strip())
        if key:
            os.environ.setdefault(key, value)


def _strip_env_value(value: str) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


_load_dotenv(SDK_COMPAT_ROOT / ".env")


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
    group.addoption(
        "--sdk-e2e-trace",
        action="store_true",
        default=False,
        help="print every SDK adapter operation and include traces for passed tests",
    )


def pytest_configure(config: pytest.Config) -> None:
    for marker in (
        "sdk_compat: SDK compatibility E2E tests",
        "requires_capability(name): current SDK backend must support this capability",
        "sandbox_create_options(**kwargs): SDK sandbox create options for this test",
        "sandbox_template_id(template_id): override template ID for this test or module",
        "requires_code_interpreter: test requires a stateful Code Interpreter kernel",
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
    config._sdk_e2e_template_ids = {
        template_id
        for item in items
        if (template_id := _template_id_for_node(item)) is not None
    }
    config._sdk_e2e_default_template_needed = any(
        _template_id_for_node(item) is None for item in items
    )
    if config.getoption("--run-e2e"):
        return
    skip = pytest.mark.skip(reason="live SDK E2E disabled; pass --run-e2e to run")
    for item in items:
        item.add_marker(skip)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item: pytest.Item, call: pytest.CallInfo):
    outcome = yield
    report = outcome.get_result()
    setattr(item, f"rep_{report.when}", report)
    if report.when not in {"setup", "call", "teardown"}:
        return
    if report.when == "setup" and report.passed:
        return

    reporter = item.funcargs.get("sdk_e2e_reporter")
    if reporter is None:
        return

    adapter = item.funcargs.get("sdk_sandbox")
    trace = item.funcargs.get("sdk_e2e_trace")
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
        if trace is not None:
            payload["trace"] = trace.snapshot()
            if not getattr(item, "_sdk_e2e_trace_dumped", False):
                terminal = item.config.pluginmanager.get_plugin("terminalreporter")
                if terminal is not None:
                    terminal.write_sep("=", "SDK E2E trace")
                    terminal.write_line(trace.format_failure())
                item._sdk_e2e_trace_dumped = True
        if adapter is not None:
            try:
                payload["sandbox_info"] = adapter.info().raw
            except Exception as exc:  # noqa: BLE001 - diagnostics must not hide the failure
                payload["sandbox_info_error"] = str(exc)
    elif report.skipped:
        payload["reason"] = str(report.longrepr)
    elif report.when == "call" and trace is not None and trace.verbose:
        payload["trace"] = trace.snapshot()

    reporter.record_test_result(**payload)


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    reporter = getattr(session.config, "_sdk_e2e_reporter", None)
    if reporter is not None:
        reporter.close()


@pytest.fixture(scope="session")
def sdk_e2e_config(pytestconfig: pytest.Config) -> SdkE2EConfig:
    cfg = _config_from_pytest(pytestconfig)
    if cfg.cube_python_sdk_path:
        sys.path.insert(0, cfg.cube_python_sdk_path)
    else:
        sys.path.insert(0, str(ROOT / "sdk" / "python"))
    for key, value in cfg.env().items():
        os.environ.setdefault(key, value)
    if pytestconfig.getoption("--run-e2e"):
        _log_effective_environment(cfg)
    return cfg


@pytest.fixture(scope="session")
def sdk_e2e_reporter(
    sdk_e2e_config: SdkE2EConfig,
    pytestconfig: pytest.Config,
) -> JsonlReporter:
    reporter = JsonlReporter(sdk_e2e_config.report_dir)
    pytestconfig._sdk_e2e_reporter = reporter
    return reporter


@pytest.fixture()
def sdk_e2e_trace(request: pytest.FixtureRequest, pytestconfig: pytest.Config) -> TraceCollector:
    verbose = pytestconfig.getoption("--sdk-e2e-trace") or _env_true("SDK_E2E_TRACE")
    terminal = pytestconfig.pluginmanager.get_plugin("terminalreporter")

    def _emit(message: str) -> None:
        if terminal is not None:
            terminal.write_line(message)

    return TraceCollector(
        request.node.nodeid,
        verbose=verbose,
        emit=_emit,
    )


@pytest.fixture(scope="session", autouse=True)
def sdk_e2e_preflight(pytestconfig: pytest.Config, sdk_e2e_config: SdkE2EConfig, sdk_e2e_reporter: JsonlReporter):
    if not pytestconfig.getoption("--run-e2e"):
        return
    try:
        run_preflight(
            sdk_e2e_config,
            sdk_e2e_reporter,
            template_ids=getattr(pytestconfig, "_sdk_e2e_template_ids", set()),
            require_default_template=getattr(
                pytestconfig,
                "_sdk_e2e_default_template_needed",
                True,
            ),
        )
    except RuntimeError as exc:
        pytest.exit(str(exc), returncode=2)


@pytest.fixture()
def sdk_sandbox(
    request: pytest.FixtureRequest,
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
    sdk_e2e_reporter: JsonlReporter,
    sdk_e2e_trace: TraceCollector,
):
    for marker in request.node.iter_markers("requires_capability"):
        capability = marker.args[0]
        if capability not in _capabilities_for_backend(sdk_backend):
            pytest.skip(f"backend {sdk_backend!r} does not support capability {capability!r}")

    if request.node.get_closest_marker("requires_code_interpreter"):
        if CODE_INTERPRETER not in _capabilities_for_backend(sdk_backend):
            pytest.skip(
                f"backend {sdk_backend!r} does not support stateful Code Interpreter"
            )

    if request.node.get_closest_marker("requires_internet") and _env_true(
        "SDK_E2E_SKIP_INTERNET_TESTS"
    ):
        pytest.skip("internet-dependent SDK E2E tests disabled by SDK_E2E_SKIP_INTERNET_TESTS")

    if request.node.get_closest_marker("requires_cubeproxy") and not sdk_e2e_config.platform_lifecycle_enabled:
        pytest.skip(
            "platform lifecycle tests require SDK_E2E_PLATFORM_LIFECYCLE=true "
            "(cube-proxy + lifecycle manager coordination)"
        )

    template_id = _template_id_for_node(request.node) or sdk_e2e_config.cube_template_id
    if not template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for SDK E2E")
    node_config = replace(sdk_e2e_config, cube_template_id=template_id)

    create_options = _create_options_for_node(request.node)

    metadata = {
        "test_suite": "sdk_compat",
        "test_backend": sdk_backend,
        "test_nodeid": request.node.nodeid,
        "test_run_id": uuid.uuid4().hex,
    }
    if request.node.get_closest_marker("requires_cubeproxy"):
        create_options.setdefault("timeout", node_config.platform_lifecycle_idle_timeout)
    request.node._sdk_e2e_backend = sdk_backend
    trace_token = set_current_trace(sdk_e2e_trace)
    adapter = None
    try:
        try:
            _setup_log(
                f"creating sandbox backend={sdk_backend} "
                f"template_id={node_config.cube_template_id} "
                f"nodeid={request.node.nodeid}"
            )
            adapter = create_adapter(
                sdk_backend,
                node_config,
                metadata=metadata,
                create_options=create_options,
            )
        except ImportError as exc:
            pytest.skip(str(exc))
        try:
            request.node._sdk_e2e_sandbox_id = adapter.sandbox_id
            _setup_log(
                f"sandbox created backend={sdk_backend} "
                f"sandbox_id={adapter.sandbox_id}"
            )

            sdk_e2e_reporter.record(
                "sandbox_created",
                backend=sdk_backend,
                sandbox_id=adapter.sandbox_id,
                nodeid=request.node.nodeid,
            )
            yield adapter
        finally:
            _cleanup_sdk_sandbox(
                adapter,
                request,
                sdk_backend,
                node_config,
                sdk_e2e_reporter,
            )
    finally:
        reset_current_trace(trace_token)


def _config_from_pytest(config: pytest.Config) -> SdkE2EConfig:
    return SdkE2EConfig.from_env(
        backends=config.getoption("--sdk-e2e-backends"),
        cube_api_url=config.getoption("--cube-api-url"),
        cube_template_id=config.getoption("--cube-template-id"),
    )


def _capabilities_for_backend(backend: str) -> frozenset[str]:
    return capabilities_for_backend(backend)


def _create_options_for_node(node: pytest.Item) -> dict:
    create_options: dict = {}
    for marker in node.iter_markers("sandbox_create_options"):
        create_options.update(marker.kwargs)
        if marker.args:
            if len(marker.args) != 1 or not isinstance(marker.args[0], dict):
                raise ValueError("sandbox_create_options accepts keyword arguments or one dict argument")
            create_options.update(marker.args[0])
    return create_options


def _template_id_for_node(node: pytest.Item) -> str | None:
    marker = node.get_closest_marker("sandbox_template_id")
    if marker is None:
        return None
    if marker.args and marker.kwargs:
        raise ValueError("sandbox_template_id accepts either one positional or one keyword template_id value")
    if marker.kwargs:
        if set(marker.kwargs) != {"template_id"}:
            raise ValueError("sandbox_template_id accepts one template_id value")
        template_id = marker.kwargs["template_id"]
    else:
        if len(marker.args) != 1:
            raise ValueError("sandbox_template_id accepts one template_id value")
        template_id = marker.args[0]
    if not isinstance(template_id, str) or not template_id.strip():
        raise ValueError("sandbox_template_id requires a non-empty string")
    return template_id.strip()


def _cleanup_sdk_sandbox(
    adapter,
    request: pytest.FixtureRequest,
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
    sdk_e2e_reporter: JsonlReporter,
) -> None:
    if adapter is None:
        return
    failed = _test_failed(request.node)
    if sdk_e2e_config.keep_sandbox_on_failure and failed:
        errors: list[str] = []
        try:
            adapter.close()
        except Exception as exc:  # noqa: BLE001 - keep remote sandbox, close local handles best-effort
            errors.append(f"{adapter.backend}.close failed for {adapter.sandbox_id}: {exc}")
        sdk_e2e_reporter.record(
            "sandbox_kept",
            backend=sdk_backend,
            sandbox_id=adapter.sandbox_id,
            nodeid=request.node.nodeid,
            reason="test_failed",
            errors=errors,
        )
        return

    errors = safe_kill(adapter, sdk_e2e_config)
    sdk_e2e_reporter.record(
        "sandbox_cleanup",
        backend=sdk_backend,
        sandbox_id=adapter.sandbox_id,
        nodeid=request.node.nodeid,
        errors=errors,
    )


def _test_failed(node: pytest.Item) -> bool:
    return any(
        getattr(report, "failed", False)
        for report in (
            getattr(node, "rep_setup", None),
            getattr(node, "rep_call", None),
        )
    )


def _env_true(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def _log_effective_environment(cfg: SdkE2EConfig) -> None:
    fields = {
        "SDK_E2E_BACKENDS": ",".join(cfg.backends),
        "CUBE_API_URL": cfg.cube_api_url,
        "CUBE_TEMPLATE_ID": cfg.cube_template_id,
        "CUBE_API_KEY": _presence("CUBE_API_KEY"),
        "CUBE_PROXY_NODE_IP": cfg.cube_proxy_node_ip,
        "CUBE_PROXY_PORT_HTTP": str(cfg.cube_proxy_port_http),
        "CUBE_SANDBOX_DOMAIN": cfg.cube_sandbox_domain,
        "SDK_E2E_PLATFORM_LIFECYCLE": str(cfg.platform_lifecycle_enabled).lower(),
    }
    if "e2b" in cfg.backends:
        fields.update(
            {
                "E2B_EFFECTIVE_API_URL": cfg.cube_api_url,
                "E2B_API_KEY": _e2b_api_key_source(),
                "SDK_E2E_E2B_VALIDATE_API_KEY": str(cfg.e2b_validate_api_key).lower(),
                "SSL_CERT_FILE": os.environ.get("SSL_CERT_FILE"),
            }
        )
    summary = " ".join(
        f"{key}={_display_env_value(value)}" for key, value in fields.items()
    )
    _setup_log(f"effective env {summary}")


def _display_env_value(value: str | None) -> str:
    return value if value else "<unset>"


def _presence(name: str) -> str:
    return "<set>" if os.environ.get(name) else "<unset>"


def _e2b_api_key_source() -> str:
    if os.environ.get("E2B_API_KEY"):
        return "<set from E2B_API_KEY>"
    return "<missing E2B_API_KEY>"


def _setup_log(message: str) -> None:
    timestamp = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
    print(f"[sdk-e2e][{timestamp}] {message}", flush=True)
