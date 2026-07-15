# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


def _csv(value: str) -> list[str]:
    return [item.strip() for item in value.split(",") if item.strip()]


def _bool_env(name: str, default: bool = False) -> bool:
    value = os.environ.get(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class SdkE2EConfig:
    backends: tuple[str, ...]
    cube_api_url: str
    cube_template_id: str | None
    cube_proxy_node_ip: str | None
    cube_proxy_port_http: int
    cube_sandbox_domain: str
    default_timeout: int
    api_timeout: float
    create_timeout: int
    command_timeout: int
    run_code_timeout: int
    network_probe_timeout: int
    e2b_validate_api_key: bool
    keep_sandbox_on_failure: bool
    report_dir: Path
    cube_python_sdk_path: str | None
    platform_lifecycle_enabled: bool
    platform_lifecycle_idle_timeout: int
    platform_lifecycle_wait_margin: int
    platform_lifecycle_poll_timeout: int
    cube_proxy_admin_port: int

    @classmethod
    def from_env(
        cls,
        *,
        backends: str | None = None,
        cube_api_url: str | None = None,
        cube_template_id: str | None = None,
    ) -> "SdkE2EConfig":
        selected_backends = tuple(
            _csv(backends or os.environ.get("SDK_E2E_BACKENDS", "cubesandbox"))
        )
        resolved_cube_api_url = (
            cube_api_url or os.environ.get("CUBE_API_URL") or "http://127.0.0.1:3000"
        ).rstrip("/")
        return cls(
            backends=selected_backends,
            cube_api_url=resolved_cube_api_url,
            cube_template_id=cube_template_id or os.environ.get("CUBE_TEMPLATE_ID"),
            cube_proxy_node_ip=os.environ.get("CUBE_PROXY_NODE_IP") or None,
            cube_proxy_port_http=int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80")),
            cube_sandbox_domain=os.environ.get("CUBE_SANDBOX_DOMAIN", "cube.app"),
            default_timeout=int(os.environ.get("SDK_E2E_DEFAULT_TIMEOUT", "120")),
            api_timeout=float(os.environ.get("SDK_E2E_API_TIMEOUT", "5")),
            create_timeout=int(os.environ.get("SDK_E2E_CREATE_TIMEOUT", "120")),
            command_timeout=int(os.environ.get("SDK_E2E_COMMAND_TIMEOUT", "30")),
            run_code_timeout=int(os.environ.get("SDK_E2E_RUN_CODE_TIMEOUT", "60")),
            network_probe_timeout=int(os.environ.get("SDK_E2E_NETWORK_PROBE_TIMEOUT", "5")),
            e2b_validate_api_key=_bool_env("SDK_E2E_E2B_VALIDATE_API_KEY"),
            keep_sandbox_on_failure=_bool_env("SDK_E2E_KEEP_SANDBOX_ON_FAILURE"),
            report_dir=Path(os.environ.get("SDK_E2E_REPORT_DIR", "reports/sdk-dual")),
            cube_python_sdk_path=os.environ.get("CUBE_PYTHON_SDK_PATH") or None,
            platform_lifecycle_enabled=_bool_env("SDK_E2E_PLATFORM_LIFECYCLE"),
            platform_lifecycle_idle_timeout=int(
                os.environ.get("SDK_E2E_PLATFORM_LIFECYCLE_IDLE_TIMEOUT", "30")
            ),
            platform_lifecycle_wait_margin=int(
                os.environ.get("SDK_E2E_PLATFORM_LIFECYCLE_WAIT_MARGIN", "20")
            ),
            platform_lifecycle_poll_timeout=int(
                os.environ.get("SDK_E2E_PLATFORM_LIFECYCLE_POLL_TIMEOUT", "45")
            ),
            cube_proxy_admin_port=int(os.environ.get("CUBE_PROXY_ADMIN_PORT", "8082")),
        )

    def env(self) -> dict[str, str]:
        values = {
            "CUBE_API_URL": self.cube_api_url,
            "CUBE_PROXY_PORT_HTTP": str(self.cube_proxy_port_http),
            "CUBE_SANDBOX_DOMAIN": self.cube_sandbox_domain,
        }
        if self.cube_template_id:
            values["CUBE_TEMPLATE_ID"] = self.cube_template_id
        if self.cube_proxy_node_ip:
            values["CUBE_PROXY_NODE_IP"] = self.cube_proxy_node_ip
        return values
