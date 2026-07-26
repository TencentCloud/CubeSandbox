# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from typing import Any

from adapters.base import SandboxAdapter, cleanup_raw_sandbox
from framework.capabilities import CUBESANDBOX_CAPABILITIES
from framework.config import SdkE2EConfig
from framework.models import CodeResult, CommandResult, SandboxInfo, state_from_raw


class CubeSandboxAdapter(SandboxAdapter):
    backend = "cubesandbox"
    capabilities = CUBESANDBOX_CAPABILITIES

    @classmethod
    def create(
        cls,
        config: SdkE2EConfig,
        *,
        metadata: dict[str, str] | None = None,
        create_options: dict[str, Any] | None = None,
    ) -> "CubeSandboxAdapter":
        from cubesandbox import Config, Sandbox

        sdk_config = cls._sdk_config(config)
        opts = dict(create_options or {})
        merged_metadata = dict(metadata or {})
        extra_metadata = opts.pop("metadata", None)
        if isinstance(extra_metadata, dict):
            merged_metadata.update(extra_metadata)
        timeout = opts.pop("timeout", config.create_timeout)
        sandbox = None
        try:
            sandbox = Sandbox.create(
                timeout=timeout,
                metadata=merged_metadata or None,
                config=sdk_config,
                **opts,
            )
            return cls(sandbox, sdk_config=sdk_config, e2e_config=config)
        except Exception:
            if sandbox is not None:
                cleanup_raw_sandbox(sandbox)
            raise

    @classmethod
    def connect(
        cls,
        sandbox_id: str,
        config: SdkE2EConfig,
    ) -> "CubeSandboxAdapter":
        from cubesandbox import Sandbox

        sdk_config = cls._sdk_config(config)
        return cls(Sandbox.connect(sandbox_id, config=sdk_config), sdk_config=sdk_config, e2e_config=config)

    @classmethod
    def list_sandboxes(cls, config: SdkE2EConfig) -> list[dict[str, Any]]:
        from cubesandbox import Sandbox

        return Sandbox.list(config=cls._sdk_config(config))

    @staticmethod
    def _sdk_config(config: SdkE2EConfig):
        from cubesandbox import Config

        return Config(
            api_url=config.cube_api_url,
            template_id=config.cube_template_id,
            proxy_node_ip=config.cube_proxy_node_ip,
            proxy_port=config.cube_proxy_port_http,
            sandbox_domain=config.cube_sandbox_domain,
        )

    def __init__(self, sandbox: Any, *, sdk_config: Any, e2e_config: SdkE2EConfig | None = None) -> None:
        super().__init__(sandbox)
        self._sdk_config = sdk_config
        self._e2e_config = e2e_config

    @property
    def sandbox_id(self) -> str:
        return self._sandbox.sandbox_id

    def info(self) -> SandboxInfo:
        raw = self._sandbox.get_info()
        sandbox_id = raw["sandboxID"] if "sandboxID" in raw else self.sandbox_id
        return SandboxInfo(
            sandbox_id=sandbox_id,
            state=state_from_raw(raw),
            raw=raw,
        )

    def run_command(self, command: str, *, user: str = "root", timeout: int = 30) -> CommandResult:
        result = self._sandbox.commands.run(command, user=user, timeout=timeout)
        return CommandResult(
            stdout=result.stdout,
            stderr=result.stderr,
            exit_code=result.exit_code,
        )

    def write_file(self, path: str, content: str, *, user: str = "root") -> None:
        self._sandbox.files.write(path, content, user=user)

    def read_file(self, path: str, *, user: str = "root") -> str:
        return self._sandbox.files.read(path, user=user)

    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        execution = self._sandbox.run_code(code, timeout=timeout)
        stdout = _normalize_log_lines(execution.logs.stdout) if execution.logs else []
        stderr = _normalize_log_lines(execution.logs.stderr) if execution.logs else []
        return CodeResult(
            text=execution.text,
            stdout=stdout,
            stderr=stderr,
            error=execution.error,
        )

    def pause(self, *, timeout: int = 60) -> None:
        self._sandbox.pause(timeout=timeout)

    def resume_or_connect(self, *, timeout: int = 60) -> "CubeSandboxAdapter":
        return type(self).connect(self.sandbox_id, self._e2e_config or SdkE2EConfig.from_env())

    def get_host(self, port: int) -> str:
        return str(self._sandbox.get_host(port))

    def traffic_access_token(self) -> str | None:
        token = getattr(self._sandbox, "traffic_access_token", None)
        if token:
            return str(token)
        try:
            raw = self.info().raw
        except Exception:  # noqa: BLE001 - token lookup should degrade gracefully
            return None
        for key in ("traffic_access_token", "trafficAccessToken"):
            if key in raw and raw[key]:
                return str(raw[key])
        return None

    def kill(self) -> None:
        self._sandbox.kill()


def _normalize_log_lines(items: Any) -> list[str]:
    return [str(getattr(item, "line", item)) for item in items or []]
