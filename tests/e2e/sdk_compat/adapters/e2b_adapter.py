# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
from typing import Any

from framework.capabilities import E2B_CAPABILITIES
from framework.config import SdkE2EConfig
from framework.exceptions import UnsupportedCapability
from framework.models import CodeResult, CommandResult, DirEntry, SandboxInfo

from adapters.base import SandboxAdapter


def _import_e2b_sandbox():
    try:
        from e2b_code_interpreter import Sandbox  # type: ignore

        return Sandbox
    except ImportError:
        pass
    try:
        from e2b import Sandbox  # type: ignore

        return Sandbox
    except ImportError as exc:
        raise ImportError(
            "E2B backend requires e2b-code-interpreter or e2b. "
            "Install tests/e2e/sdk_compat/requirements.txt."
        ) from exc


def _get_sandbox_id(sandbox: Any) -> str:
    for name in ("sandbox_id", "sandboxID", "id"):
        value = getattr(sandbox, name, None)
        if value:
            return str(value)
    data = getattr(sandbox, "_data", None)
    if isinstance(data, dict):
        return str(data.get("sandboxID") or data.get("sandbox_id") or data.get("id"))
    raise RuntimeError("could not determine E2B sandbox id")


class E2BAdapter(SandboxAdapter):
    backend = "e2b"
    capabilities = E2B_CAPABILITIES

    @classmethod
    def create(cls, config: SdkE2EConfig, *, metadata: dict[str, str] | None = None) -> E2BAdapter:
        Sandbox = _import_e2b_sandbox()
        _configure_e2b_transport(config)
        os.environ.setdefault("E2B_API_URL", config.cube_api_url)
        os.environ.setdefault("E2B_API_KEY", os.environ.get("CUBE_API_KEY", "dummy"))

        kwargs: dict[str, Any] = {
            "template": config.cube_template_id,
            "timeout": config.create_timeout,
            "metadata": metadata,
        }
        kwargs = {key: value for key, value in kwargs.items() if value is not None}
        try:
            sandbox = Sandbox.create(**kwargs)
        except AttributeError:
            sandbox = Sandbox(**kwargs)
        return cls(sandbox)

    @classmethod
    def connect(cls, sandbox_id: str, config: SdkE2EConfig) -> E2BAdapter:
        Sandbox = _import_e2b_sandbox()
        _configure_e2b_transport(config)
        os.environ.setdefault("E2B_API_URL", config.cube_api_url)
        try:
            sandbox = Sandbox.connect(sandbox_id)
        except AttributeError:
            sandbox = Sandbox(sandbox_id=sandbox_id)
        return cls(sandbox)

    @property
    def sandbox_id(self) -> str:
        return _get_sandbox_id(self._sandbox)

    def info(self) -> SandboxInfo:
        raw: dict[str, Any] = {}
        for name in ("get_info", "get_info_sync"):
            method = getattr(self._sandbox, name, None)
            if callable(method):
                maybe_raw = method()
                if isinstance(maybe_raw, dict):
                    raw = maybe_raw
                break
        return SandboxInfo(
            sandbox_id=raw.get("sandboxID") or raw.get("sandbox_id") or self.sandbox_id,
            state=raw.get("state"),
            raw=raw,
        )

    def run_command(self, command: str, *, user: str = "root", timeout: int = 30) -> CommandResult:
        commands = getattr(self._sandbox, "commands", None)
        if commands is None:
            raise RuntimeError("E2B sandbox object does not expose commands")
        try:
            try:
                result = commands.run(command, timeout=timeout, user=user)
            except TypeError:
                result = commands.run(command, timeout=timeout)
        except Exception as exc:  # E2B raises on non-zero command exits.
            if not hasattr(exc, "exit_code"):
                raise
            return CommandResult(
                stdout=str(getattr(exc, "stdout", "") or ""),
                stderr=str(getattr(exc, "stderr", "") or ""),
                exit_code=int(getattr(exc, "exit_code")),
            )
        return CommandResult(
            stdout=str(getattr(result, "stdout", "") or ""),
            stderr=str(getattr(result, "stderr", "") or ""),
            exit_code=int(getattr(result, "exit_code", getattr(result, "exitCode", 0))),
        )

    def write_file(self, path: str, content: str, *, user: str = "root") -> None:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        writer = getattr(files, "write", None) or getattr(files, "write_file", None)
        if not callable(writer):
            raise RuntimeError("E2B files object does not expose write/write_file")
        writer(path, content)

    def read_file(self, path: str, *, user: str = "root") -> str:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        reader = getattr(files, "read", None) or getattr(files, "read_file", None)
        if not callable(reader):
            raise RuntimeError("E2B files object does not expose read/read_file")
        return str(reader(path))

    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        result = self._sandbox.run_code(code, timeout=timeout)
        logs = getattr(result, "logs", None)
        stdout = list(getattr(logs, "stdout", []) or [])
        stderr = list(getattr(logs, "stderr", []) or [])
        return CodeResult(
            text=str(getattr(result, "text", "") or ""),
            stdout=[str(item) for item in stdout],
            stderr=[str(item) for item in stderr],
            error=getattr(result, "error", None),
        )

    def exists(self, path: str) -> bool:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        exists = getattr(files, "exists", None)
        if callable(exists):
            return bool(exists(path))
        # Fallback: stat-style probe.
        stat = getattr(files, "stat", None)
        if callable(stat):
            try:
                stat(path)
                return True
            except Exception:
                return False
        raise RuntimeError("E2B files object does not expose exists/stat")

    def list_dir(self, path: str) -> list[DirEntry]:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        lister = getattr(files, "list", None) or getattr(files, "list_dir", None)
        if not callable(lister):
            raise RuntimeError("E2B files object does not expose list/list_dir")
        entries = lister(path)
        result: list[DirEntry] = []
        for e in entries:
            result.append(
                DirEntry(
                    name=str(getattr(e, "name", e.get("name", "")) if isinstance(e, dict) else getattr(e, "name", "")),
                    is_dir=bool(
                        getattr(e, "is_dir", None)
                        or (e.get("is_dir") if isinstance(e, dict) else None)
                        or (e.get("isDir") if isinstance(e, dict) else None)
                    ),
                )
            )
        return result

    def make_dir(self, path: str) -> None:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        maker = getattr(files, "make_dir", None) or getattr(files, "mkdir", None)
        if not callable(maker):
            raise RuntimeError("E2B files object does not expose make_dir/mkdir")
        maker(path)

    def remove_file(self, path: str) -> None:
        files = getattr(self._sandbox, "files", None)
        if files is None:
            raise RuntimeError("E2B sandbox object does not expose files")
        remover = getattr(files, "remove", None) or getattr(files, "delete", None)
        if not callable(remover):
            raise RuntimeError("E2B files object does not expose remove/delete")
        remover(path)

    def set_timeout(self, timeout: int) -> None:
        setter = getattr(self._sandbox, "set_timeout", None)
        if callable(setter):
            setter(timeout)
            return
        raise UnsupportedCapability(self.backend, "set_timeout")

    def kill(self) -> None:
        for name in ("kill", "delete", "close"):
            method = getattr(self._sandbox, name, None)
            if callable(method):
                method()
                return


def _configure_e2b_transport(config: SdkE2EConfig) -> None:
    if not config.e2b_insecure_tls:
        return

    from e2b.api import limits  # type: ignore
    from e2b.api.client_sync import TransportWithLogger  # type: ignore

    # Self-hosted CubeSandbox test deployments often expose sandbox envd/Jupyter
    # through HTTPS with a locally trusted or self-signed certificate.
    TransportWithLogger.singleton = TransportWithLogger(
        limits=limits,
        verify=False,
    )
