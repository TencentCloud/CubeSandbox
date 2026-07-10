# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import Any

from framework.exceptions import UnsupportedCapability
from framework.models import CodeResult, CommandResult, DirEntry, SandboxInfo


class SandboxAdapter(ABC):
    backend: str
    capabilities: frozenset[str]

    def __init__(self, sandbox: Any) -> None:
        self._sandbox = sandbox

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

    def exists(self, path: str) -> bool:
        raise UnsupportedCapability(self.backend, "filesystem")

    def list_dir(self, path: str) -> list[DirEntry]:
        raise UnsupportedCapability(self.backend, "filesystem")

    def make_dir(self, path: str) -> None:
        raise UnsupportedCapability(self.backend, "filesystem")

    def remove_file(self, path: str) -> None:
        raise UnsupportedCapability(self.backend, "filesystem")

    def set_timeout(self, timeout: int) -> None:
        raise UnsupportedCapability(self.backend, "set_timeout")

    def pause(self, *, timeout: int = 60) -> None:
        raise UnsupportedCapability(self.backend, "pause_resume")

    def resume_or_connect(self, *, timeout: int = 60) -> SandboxAdapter:
        raise UnsupportedCapability(self.backend, "pause_resume")

    @abstractmethod
    def kill(self) -> None:
        raise NotImplementedError

    def close(self) -> None:
        close = getattr(self._sandbox, "close", None)
        if callable(close):
            close()
