# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from typing import Any

from framework.capabilities import CUBESANDBOX_CAPABILITIES
from framework.config import SdkE2EConfig
from framework.models import CodeResult, CommandResult, DirEntry, SandboxInfo

from adapters.base import SandboxAdapter


class CubeSandboxAdapter(SandboxAdapter):
    backend = "cubesandbox"
    capabilities = CUBESANDBOX_CAPABILITIES

    @classmethod
    def create(cls, config: SdkE2EConfig, *, metadata: dict[str, str] | None = None) -> CubeSandboxAdapter:
        from cubesandbox import Config, Sandbox

        sdk_config = Config(
            api_url=config.cube_api_url,
            template_id=config.cube_template_id,
            proxy_node_ip=config.cube_proxy_node_ip,
            proxy_port=config.cube_proxy_port_http,
            sandbox_domain=config.cube_sandbox_domain,
        )
        sandbox = Sandbox.create(
            timeout=config.create_timeout,
            metadata=metadata,
            config=sdk_config,
        )
        return cls(sandbox, sdk_config=sdk_config, e2e_config=config)

    @classmethod
    def connect(
        cls,
        sandbox_id: str,
        config: SdkE2EConfig,
        *,
        timeout: int | None = None,
    ) -> CubeSandboxAdapter:
        from cubesandbox import Config, Sandbox

        sdk_config = Config(
            api_url=config.cube_api_url,
            template_id=config.cube_template_id,
            proxy_node_ip=config.cube_proxy_node_ip,
            proxy_port=config.cube_proxy_port_http,
            sandbox_domain=config.cube_sandbox_domain,
        )
        return cls(Sandbox.connect(sandbox_id, timeout=timeout, config=sdk_config), sdk_config=sdk_config, e2e_config=config)

    def __init__(self, sandbox: Any, *, sdk_config: Any, e2e_config: SdkE2EConfig | None = None) -> None:
        super().__init__(sandbox)
        self._sdk_config = sdk_config
        self._e2e_config = e2e_config

    @property
    def sandbox_id(self) -> str:
        return self._sandbox.sandbox_id

    def info(self) -> SandboxInfo:
        raw = self._sandbox.get_info()
        return SandboxInfo(
            sandbox_id=raw.get("sandboxID") or self.sandbox_id,
            state=raw.get("state"),
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

    def exists(self, path: str) -> bool:
        return self._sandbox.files.exists(path)

    def list_dir(self, path: str) -> list[DirEntry]:
        entries = self._sandbox.files.list(path)
        return [
            DirEntry(name=str(e.get("name", "")), is_dir=bool(e.get("isDir", False)))
            for e in entries
        ]

    def make_dir(self, path: str) -> None:
        self._sandbox.files.make_dir(path)

    def remove_file(self, path: str) -> None:
        self._sandbox.files.remove(path)

    def set_timeout(self, timeout: int) -> None:
        self._sandbox.set_timeout(timeout)

    def pause(self, *, timeout: int = 60) -> None:
        self._sandbox.pause(timeout=timeout)

    def resume_or_connect(self, *, timeout: int = 60) -> CubeSandboxAdapter:
        return type(self).connect(self.sandbox_id, self._e2e_config or SdkE2EConfig.from_env(), timeout=timeout)

    def kill(self) -> None:
        self._sandbox.kill()


def _normalize_log_lines(items: Any) -> list[str]:
    return [str(getattr(item, "line", item)) for item in items or []]
