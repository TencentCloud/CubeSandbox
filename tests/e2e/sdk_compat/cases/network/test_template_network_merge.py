# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Template CubeNetworkConfig merged with per-create network options."""

from __future__ import annotations

import os
import time

import pytest

from adapters import create_adapter
from framework.build_throttle import template_build_slot
from framework.capabilities import NETWORK_TEMPLATE_MERGE, capabilities_for_backend
from framework.cleanup import safe_kill
from framework.config import SdkE2EConfig
from framework.parallel import scale_timeout_for_xdist
from framework.network_probe import (
    assert_tcp_blocked,
    assert_tcp_reachable,
    tcp_probe_command,
)


def _env_true(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}

TEMPLATE_ALLOW_IP = os.environ.get("SDK_E2E_TEMPLATE_MERGE_ALLOW_IP", "8.8.8.8")
CREATE_ALLOW_IP = os.environ.get("SDK_E2E_TEMPLATE_MERGE_CREATE_ALLOW_IP", "1.1.1.1")
BLOCKED_IP = os.environ.get("SDK_E2E_TEMPLATE_MERGE_BLOCKED_IP", "9.9.9.9")
TCP_PORT = int(os.environ.get("SDK_E2E_TEMPLATE_MERGE_TCP_PORT", "53"))

DEFAULT_TEMPLATE_IMAGE = (
    "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
)
DEFAULT_WRITABLE_LAYER_SIZE = "1G"
TEMPLATE_READY_TIMEOUT = 300
# Optional guest nameserver(s). Some lab networks cannot reach the image default
# resolver (e.g. 119.29.29.29); pin a reachable one via SDK_E2E_GUEST_DNS.
def _guest_dns() -> list[str] | None:
    raw = os.environ.get("SDK_E2E_GUEST_DNS", "").strip()
    if not raw:
        return None
    return [part.strip() for part in raw.split(",") if part.strip()]

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    # Documented intent; real gates are the autouse fixture + module fixture
    # below because this module uses create_adapter, not sdk_sandbox.
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_TEMPLATE_MERGE),
]


# Mirrors cases/volume/conftest.py: markers alone only gate sdk_sandbox.
@pytest.fixture(autouse=True)
def _gate_template_merge_case(sdk_backend: str) -> None:
    if NETWORK_TEMPLATE_MERGE not in capabilities_for_backend(sdk_backend):
        pytest.skip(
            f"backend {sdk_backend!r} does not support capability "
            f"{NETWORK_TEMPLATE_MERGE!r}"
        )
    if _env_true("SDK_E2E_SKIP_INTERNET_TESTS"):
        pytest.skip(
            "internet-dependent SDK E2E tests disabled by SDK_E2E_SKIP_INTERNET_TESTS"
        )


def _template_image() -> str:
    return (
        os.environ.get("SDK_E2E_TEMPLATE_MERGE_IMAGE")
        or os.environ.get("CUBE_TEMPLATE_E2E_IMAGE")
        or DEFAULT_TEMPLATE_IMAGE
    )


def _wait_for_template_ready(template_id: str, config, timeout: int | None = None):
    from cubesandbox import Template

    # Widen the serial-run budget under xdist (same-node build contention).
    if timeout is None:
        timeout = scale_timeout_for_xdist(TEMPLATE_READY_TIMEOUT)
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            info = Template.get(template_id, config=config)
            if info.status in ("READY", "FAILED"):
                return info
        except Exception:
            pass
        time.sleep(2)
    # Raise Exception (not pytest.fail / BaseException) so the module fixture
    # can stash the error and let non-cubesandbox params skip cleanly.
    raise TimeoutError(
        f"template {template_id} did not reach READY within {timeout}s"
    )


def _delete_template(template_id: str, config) -> None:
    from cubesandbox import Template
    from cubesandbox._exceptions import ApiError, TemplateNotFoundError

    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            Template.delete(template_id, config=config)
            return
        except TemplateNotFoundError:
            return
        except ApiError as exc:
            if "attempt is already in progress" in str(exc):
                time.sleep(5)
                continue
            raise


# Stashed when module fixture build fails so e2b params can still skip instead
# of erroring during fixture setup (module fixtures run before autouse gates).
_TEMPLATE_BUILD_ERROR: Exception | None = None


@pytest.fixture(scope="module")
def network_merge_template_id(pytestconfig: pytest.Config):
    """Build a template with deny-all + a single allow_out CIDR to merge at create."""
    global _TEMPLATE_BUILD_ERROR
    from cubesandbox import Config, Template

    # Module-scoped setup runs before the function autouse gate; skip here so
    # we do not build a template when internet cases are disabled.
    if _env_true("SDK_E2E_SKIP_INTERNET_TESTS"):
        pytest.skip(
            "internet-dependent SDK E2E tests disabled by SDK_E2E_SKIP_INTERNET_TESTS"
        )

    base = SdkE2EConfig.from_env(
        backends=pytestconfig.getoption("--sdk-e2e-backends"),
        cube_api_url=pytestconfig.getoption("--cube-api-url"),
        cube_template_id=pytestconfig.getoption("--cube-template-id"),
    )
    if "cubesandbox" not in base.backends:
        yield None
        return
    if NETWORK_TEMPLATE_MERGE not in capabilities_for_backend("cubesandbox"):
        yield None
        return

    sdk_config = Config(api_url=base.cube_api_url)
    created_id: str | None = None
    try:
        try:
            build_kwargs: dict = {
                "image": _template_image(),
                "writable_layer_size": os.environ.get(
                    "CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE",
                    DEFAULT_WRITABLE_LAYER_SIZE,
                ),
                "exposed_ports": [49999, 49983],
                "probe_port": 49999,
                "allow_internet_access": False,
                "allow_out": [TEMPLATE_ALLOW_IP],
                "deny_out": ["0.0.0.0/0"],
                "config": sdk_config,
            }
            guest_dns = _guest_dns()
            if guest_dns is not None:
                build_kwargs["dns"] = guest_dns
            with template_build_slot(label="template_network_merge"):
                job = Template.build(**build_kwargs)
                assert job.template_id.startswith("tpl-"), job.template_id
                created_id = job.template_id
                info = _wait_for_template_ready(created_id, sdk_config)
                assert info.status == "READY", (
                    f"template {created_id} finished with status={info.status!r}"
                )
            yield created_id
        except Exception as exc:  # noqa: BLE001 - defer to cubesandbox test body
            _TEMPLATE_BUILD_ERROR = exc
            if created_id is not None:
                try:
                    _delete_template(created_id, sdk_config)
                except Exception:
                    pass
                created_id = None
            yield None
    finally:
        if created_id is not None:
            try:
                _delete_template(created_id, sdk_config)
            except Exception:
                pass


def test_template_and_create_allow_out_merge(
    sdk_backend,
    sdk_e2e_config,
    network_merge_template_id,
):
    """Create-time allow_out is appended to template allow_out under deny-all."""
    if sdk_backend != "cubesandbox":
        pytest.skip("template network merge is CubeSandbox-specific")
    if not network_merge_template_id:
        if _TEMPLATE_BUILD_ERROR is not None:
            pytest.fail(
                f"failed to provision merge template: {_TEMPLATE_BUILD_ERROR!r}"
            )
        pytest.skip("failed to provision a template with network policy")

    adapter = None
    try:
        adapter = create_adapter(
            sdk_backend,
            sdk_e2e_config,
            metadata={
                "test_suite": "sdk_compat",
                "test_backend": sdk_backend,
                "test_case": "template_network_merge",
            },
            create_options={
                "template": network_merge_template_id,
                "network": {"allow_out": [CREATE_ALLOW_IP]},
            },
        )

        from_template = adapter.run_command(
            tcp_probe_command(
                TEMPLATE_ALLOW_IP,
                TCP_PORT,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_reachable(from_template, TEMPLATE_ALLOW_IP, TCP_PORT)

        from_create = adapter.run_command(
            tcp_probe_command(
                CREATE_ALLOW_IP,
                TCP_PORT,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_reachable(from_create, CREATE_ALLOW_IP, TCP_PORT)

        blocked = adapter.run_command(
            tcp_probe_command(
                BLOCKED_IP,
                TCP_PORT,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_blocked(blocked, BLOCKED_IP, TCP_PORT)
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)
