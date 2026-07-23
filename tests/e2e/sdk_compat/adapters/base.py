# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import Any

from framework.exceptions import UnsupportedCapability
from framework.models import CodeResult, CommandResult, SandboxInfo


class SandboxAdapter(ABC):
    backend: str
    capabilities: frozenset[str]

    def __init__(self, sandbox: Any) -> None:
        self._sandbox = sandbox

    @property
    def raw_sandbox(self) -> Any:
        return self._sandbox

    @property
    @abstractmethod
    def sandbox_id(self) -> str:
        raise NotImplementedError

    def require(self, capability: str) -> None:
        if capability not in self.capabilities:
            raise UnsupportedCapability(self.backend, capability)

    @abstractmethod
    def info(self) -> SandboxInfo:
        raise NotImplementedError

    @abstractmethod
    def run_command(self, command: str, *, user: str = "root", timeout: int = 30) -> CommandResult:
        raise NotImplementedError

    @abstractmethod
    def write_file(self, path: str, content: str, *, user: str = "root") -> None:
        raise NotImplementedError

    @abstractmethod
    def read_file(self, path: str, *, user: str = "root") -> str:
        raise NotImplementedError

    @abstractmethod
    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        raise NotImplementedError

    def get_host(self, port: int) -> str:
        """Return the public virtual hostname for a sandbox port."""
        method = getattr(self.raw_sandbox, "get_host", None)
        if not callable(method):
            raise UnsupportedCapability(self.backend, "get_host")
        return str(method(port))

    def pause(self, *, timeout: int = 60) -> None:
        raise UnsupportedCapability(self.backend, "pause_resume")

    def resume_or_connect(self, *, timeout: int = 60) -> "SandboxAdapter":
        raise UnsupportedCapability(self.backend, "pause_resume")

    def get_host(self, port: int) -> str:
        raise UnsupportedCapability(self.backend, "network_public_access")

    def traffic_access_token(self) -> str | None:
        raise UnsupportedCapability(self.backend, "network_public_access")

    @abstractmethod
    def kill(self) -> None:
        raise NotImplementedError

    def close(self) -> None:
        close = getattr(self.raw_sandbox, "close", None)
        if callable(close):
            close()


def cleanup_raw_sandbox(sandbox: Any) -> None:
    for name in ("kill", "delete"):
        method = getattr(sandbox, name, None)
        if callable(method):
            try:
                method()
            except Exception:
                pass
            break
    close = getattr(sandbox, "close", None)
    if callable(close):
        try:
            close()
        except Exception:
            pass
