# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import pytest

ROOT = Path(__file__).resolve().parents[3]
SDK_PATH = Path(os.environ.get("CUBE_PYTHON_SDK_PATH", ROOT / "sdk" / "python"))
if (SDK_PATH / "cubesandbox" / "__init__.py").is_file():
    sys.path.insert(0, str(SDK_PATH))


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--run-e2e",
        action="store_true",
        default=False,
        help="run live scheduler e2e tests",
    )


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    if config.getoption("--run-e2e"):
        return
    skip = pytest.mark.skip(reason="live scheduler e2e disabled; pass --run-e2e to run")
    for item in items:
        if not item.get_closest_marker("framework"):
            item.add_marker(skip)


def _metrics_url(api_url: str) -> str:
    configured = os.environ.get("CUBEMASTER_METRICS_URL", "").strip()
    if configured:
        return configured
    parts = urlsplit(api_url)
    host = parts.hostname or "127.0.0.1"
    return urlunsplit((parts.scheme or "http", f"{host}:8089", "/metrics", "", ""))


@dataclass
class SchedulerE2EConfig:
    api_url: str
    template_id: str
    metrics_url: str
    api_key: str | None


@dataclass
class Attempt:
    sandbox: object | None
    error: BaseException | None
    success_deltas: dict[str, float]
    error_deltas: dict[str, float]
    deny_all_deltas: dict[str, float]


@dataclass
class RoutingObservation:
    unlabeled_profile: str | None = None
    factory_mode: bool | None = None
    notes: list[str] = field(default_factory=list)


@pytest.fixture(scope="session")
def scheduler_config() -> SchedulerE2EConfig:
    api_url = os.environ.get("CUBE_API_URL", "").strip()
    template_id = os.environ.get("CUBE_TEMPLATE_ID", "").strip()
    missing = [
        name
        for name, value in (("CUBE_API_URL", api_url), ("CUBE_TEMPLATE_ID", template_id))
        if not value
    ]
    if missing:
        pytest.fail(f"missing required environment: {', '.join(missing)}")
    api_key = os.environ.get("CUBE_API_KEY", "").strip() or None
    return SchedulerE2EConfig(
        api_url=api_url.rstrip("/"),
        template_id=template_id,
        metrics_url=_metrics_url(api_url),
        api_key=api_key,
    )


@pytest.fixture(scope="module")
def routing(scheduler_config: SchedulerE2EConfig):
    from cubesandbox import Config, Sandbox

    from metrics import plugin_deltas, result_deltas, scrape, success_deltas

    sdk = Config(
        api_url=scheduler_config.api_url,
        api_key=scheduler_config.api_key,
        template_id=scheduler_config.template_id,
        request_timeout=120.0,
    )
    created: list = []
    observation = RoutingObservation()

    class Session:
        def create(self, metadata: dict[str, str] | None = None):
            before = scrape(scheduler_config.metrics_url)
            error = None
            sandbox = None
            try:
                sandbox = Sandbox.create(
                    template=scheduler_config.template_id,
                    metadata=metadata,
                    timeout=120,
                    config=sdk,
                )
                created.append(sandbox)
            except Exception as exc:  # noqa: BLE001 - the caller asserts success or failure
                error = exc
            after = scrape(scheduler_config.metrics_url)
            return Attempt(
                sandbox=sandbox,
                error=error,
                success_deltas=success_deltas(before.attempts, after.attempts),
                error_deltas=result_deltas(before.attempts, after.attempts, "error"),
                deny_all_deltas=plugin_deltas(
                    before.plugin_counts,
                    after.plugin_counts,
                    plugin="deny_all",
                    kind="filter",
                ),
            )

    session = Session()
    try:
        yield session, observation
    finally:
        errors: list[str] = []
        for sandbox in created:
            try:
                sandbox.kill()
            except Exception as exc:  # noqa: BLE001 - report cleanup, do not hide the test result
                errors.append(f"{getattr(sandbox, 'sandbox_id', sandbox)}: {exc}")
        if errors:
            observation.notes.extend(errors)
