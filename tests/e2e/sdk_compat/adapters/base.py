# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Callable
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

    def list_files(self, path: str) -> list[dict[str, Any]]:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def stat_file(self, path: str) -> dict[str, Any]:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def file_exists(self, path: str) -> bool:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def remove_file(self, path: str) -> None:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def rename_file(self, old_path: str, new_path: str) -> dict[str, Any]:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def make_dir(self, path: str) -> None:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def write_files(self, files: list[tuple[str, str | bytes]]) -> int:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    def watch_dir_events(
        self,
        path: str,
        operation: Callable[[], None],
        *,
        timeout: float = 5,
        until: Callable[[list[dict[str, str]]], bool] | None = None,
    ) -> list[dict[str, str]]:
        raise UnsupportedCapability(self.backend, "filesystem_extended")

    @abstractmethod
    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        raise NotImplementedError

    def pause(self, *, timeout: int = 60) -> None:
        raise UnsupportedCapability(self.backend, "pause_resume")

    def resume_or_connect(self, *, timeout: int = 60) -> "SandboxAdapter":
        raise UnsupportedCapability(self.backend, "pause_resume")

    def set_timeout(self, timeout: int) -> None:
        raise UnsupportedCapability(self.backend, "set_timeout")

    def create_snapshot(self) -> str:
        raise UnsupportedCapability(self.backend, "rollback_clone")

    def delete_snapshot(self, snapshot_id: str) -> None:
        raise UnsupportedCapability(self.backend, "rollback_clone")

    def rollback(self, snapshot_id: str) -> dict[str, Any]:
        raise UnsupportedCapability(self.backend, "rollback_clone")

    def clone(self, n: int = 1, *, concurrency: int = 1) -> list["SandboxAdapter"]:
        raise UnsupportedCapability(self.backend, "rollback_clone")

    def list_snapshot_ids(self) -> set[str]:
        raise UnsupportedCapability(self.backend, "rollback_clone")

    def get_host(self, port: int) -> str:
        raise UnsupportedCapability(self.backend, "network_public_access")

    def traffic_access_token(self) -> str | None:
        raise UnsupportedCapability(self.backend, "network_public_access")

    def update_network(self, network: dict | None = None) -> None:
        """Replace the egress policy of the running sandbox.

        Takes the whole policy as one object, ``allow_internet_access``
        included, so the signature matches E2B's ``update_network``.
        """
        raise UnsupportedCapability(self.backend, "network_dynamic_update")

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
