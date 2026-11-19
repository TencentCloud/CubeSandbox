# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Fixtures for the live machine-type label E2E suite.

The suite talks to a running CubeOps and CubeAPI:

- CubeOps (default :3010) for node labels: ``/api/v1/nodes`` is JWT-protected,
  the ``/internal/v1`` routes are not.
- CubeAPI (default :3000) for sandbox lifecycle.

Live tests are opt-in through ``--run-e2e`` so a plain ``pytest`` run stays
hermetic (only the ``framework`` checks execute).
"""

from __future__ import annotations

import os
import sys
import uuid
from dataclasses import dataclass
from pathlib import Path

import pytest

def _find_sdk_path() -> Path | None:
    """Locate the repo's Python SDK.

    The suite may be copied elsewhere (e.g. onto a test host), so do not assume
    a fixed depth. CUBE_PYTHON_SDK_PATH wins; otherwise walk up looking for
    sdk/python/cubesandbox.
    """
    configured = os.environ.get("CUBE_PYTHON_SDK_PATH")
    if configured:
        candidate = Path(configured)
        return candidate if (candidate / "cubesandbox" / "__init__.py").is_file() else None
    for parent in Path(__file__).resolve().parents:
        candidate = parent / "sdk" / "python"
        if (candidate / "cubesandbox" / "__init__.py").is_file():
            return candidate
    return None


SDK_PATH = _find_sdk_path()
if SDK_PATH is not None:
    sys.path.insert(0, str(SDK_PATH))


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--run-e2e",
        action="store_true",
        default=False,
        help="run live machine-type label e2e tests",
    )
    parser.addoption(
        "--run-destructive",
        action="store_true",
        default=False,
        help=(
            "also run tests that restart cluster services; they temporarily make "
            "the node unschedulable (single-node clusters cannot create sandboxes "
            "until CubeMaster re-learns the template replica)"
        ),
    )


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    # Node-lifecycle tests restart Cubelet, which makes the node briefly
    # unschedulable. Run them last so they cannot race a sandbox-creating test;
    # pytest's default file ordering would otherwise interleave them.
    lifecycle = [item for item in items if item.get_closest_marker("node_lifecycle")]
    if lifecycle:
        items[:] = [item for item in items if item not in lifecycle] + lifecycle

    if not config.getoption("--run-destructive"):
        destructive = pytest.mark.skip(
            reason="restarts cluster services; pass --run-destructive to run"
        )
        for item in items:
            if item.get_closest_marker("node_lifecycle"):
                item.add_marker(destructive)

    if config.getoption("--run-e2e"):
        return
    skip = pytest.mark.skip(reason="live machine-type label e2e disabled; pass --run-e2e to run")
    for item in items:
        if not item.get_closest_marker("framework"):
            item.add_marker(skip)


@dataclass(frozen=True)
class OpsConfig:
    ops_url: str
    api_url: str
    username: str
    password: str
    template_id: str
    node_id: str


@pytest.fixture(scope="session")
def ops_config() -> OpsConfig:
    ops_url = os.environ.get("CUBEOPS_URL", "http://127.0.0.1:3010").strip().rstrip("/")
    api_url = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000").strip().rstrip("/")
    template_id = os.environ.get("CUBE_TEMPLATE_ID", "").strip()
    missing = [name for name, value in (("CUBE_TEMPLATE_ID", template_id),) if not value]
    if missing:
        pytest.fail(f"missing required environment: {', '.join(missing)}")
    return OpsConfig(
        ops_url=ops_url,
        api_url=api_url,
        username=os.environ.get("CUBEOPS_USERNAME", "admin"),
        password=os.environ.get("CUBEOPS_PASSWORD", "admin"),
        template_id=template_id,
        node_id=os.environ.get("CUBEOPS_NODE_ID", ""),
    )


@pytest.fixture(scope="session")
def ops(ops_config: OpsConfig):
    from ops_client import OpsClient

    client = OpsClient(ops_config.ops_url, ops_config.username, ops_config.password)
    client.login()
    nodes = client.list_nodes()
    if not nodes:
        pytest.fail(f"CubeOps at {ops_config.ops_url} reports no registered nodes")
    return client


@pytest.fixture(scope="module")
def node_id(ops, ops_config: OpsConfig) -> str:
    if ops_config.node_id:
        return ops_config.node_id
    nodes = ops.list_nodes()
    if not nodes:
        pytest.fail("no nodes available for the label suite")
    return nodes[0]["nodeID"]


@dataclass
class SandboxSession:
    created: list
    config: object

    def create(self, distribution_scope: list[str] | None = None):
        from cubesandbox import Sandbox

        sandbox = Sandbox.create(
            template=os.environ["CUBE_TEMPLATE_ID"],
            timeout=180,
            distribution_scope=distribution_scope,
            config=self.config,
        )
        self.created.append(sandbox)
        return sandbox

    def kill_all(self) -> list[str]:
        errors: list[str] = []
        for sandbox in self.created:
            try:
                sandbox.kill()
            except Exception as exc:  # noqa: BLE001 - report cleanup failures, keep the test result
                errors.append(f"{getattr(sandbox, 'sandbox_id', sandbox)}: {exc}")
        self.created.clear()
        return errors


@pytest.fixture(scope="module")
def sandboxes(ops_config: OpsConfig):
    from cubesandbox import Config

    # Bind the SDK to this cluster explicitly so the tests never inherit a
    # developer's local default endpoint.
    cfg = Config(
        api_url=ops_config.api_url,
        template_id=ops_config.template_id,
        request_timeout=180.0,
    )
    session = SandboxSession(created=[], config=cfg)
    try:
        yield session
    finally:
        session.kill_all()


@pytest.fixture
def unique_key() -> str:
    return f"e2e-{uuid.uuid4().hex[:8]}"


ISOLATION_KEY = "cube.cloud.tencentcloud.com/scheduling-disabled"
MACHINE_TYPE_KEY = "cube.cubesandbox.io/machine-type"

# Data-plane labels reported by Cubelet: read-only for the admin API.
SYSTEM_LABEL_PREFIXES = ("kubernetes.io/", "beta.kubernetes.io/", "cube.cloud.tencentcloud.com/")


def is_system_label(key: str) -> bool:
    return key.startswith(SYSTEM_LABEL_PREFIXES) or key in SYSTEM_LABEL_PREFIXES


def user_labels(labels: dict[str, str]) -> dict[str, str]:
    """The operator-managed subset of a node's labels."""
    return {
        k: v
        for k, v in labels.items()
        if not is_system_label(k) and k not in (ISOLATION_KEY, MACHINE_TYPE_KEY)
    }


def wait_until_schedulable(ops, node_id: str, timeout: float = 90.0) -> dict:
    """Wait until CubeOps reports the node healthy and uncordoned.

    Label writes and Cubelet re-registrations propagate asynchronously; polling
    here keeps an assertion about labels from failing on that lag.
    """
    import time

    deadline = time.time() + timeout
    last: dict = {}
    while time.time() < deadline:
        last = ops.get_node(node_id)
        if last.get("healthy") and not last.get("schedulingDisabled"):
            return last
        time.sleep(1)
    raise AssertionError(f"node {node_id} did not become schedulable: {last}")

@pytest.fixture
def clean_labels(ops, node_id):
    """Snapshot operator-managed labels and restore them after the test,
    including the previous machine-type value."""
    before = ops.labels(node_id)
    before_user = user_labels(before)
    before_machine_type = before.get(MACHINE_TYPE_KEY)
    yield
    try:
        ops.replace_labels(node_id, before_user)
        current = ops.labels(node_id)
        if before_machine_type is None:
            if MACHINE_TYPE_KEY in current:
                ops.clear_machine_type(node_id)
        else:
            ops.bind_machine_type(node_id, before_machine_type)
    except Exception as exc:  # noqa: BLE001 - cleanup failure must be visible
        pytest.fail(f"failed to restore node labels: {exc}")
