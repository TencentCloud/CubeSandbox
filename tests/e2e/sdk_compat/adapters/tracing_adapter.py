# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from adapters.base import SandboxAdapter
from framework.models import CodeResult, CommandResult, SandboxInfo
from framework.trace import TraceCollector


class TracingSandboxAdapter(SandboxAdapter):
    def __init__(self, wrapped: SandboxAdapter, trace: TraceCollector) -> None:
        super().__init__(wrapped.raw_sandbox)
        self._wrapped = wrapped
        self._trace = trace
        self.backend = wrapped.backend
        self.capabilities = wrapped.capabilities

    @property
    def sandbox_id(self) -> str:
        return self._wrapped.sandbox_id

    def info(self) -> SandboxInfo:
        return self._trace.capture(
            "info",
            {"backend": self.backend, "sandbox_id": self.sandbox_id},
            self._wrapped.info,
            output=lambda result: {
                "sandbox_id": result.sandbox_id,
                "state": result.state,
                "raw": result.raw,
            },
        )

    def run_command(
        self,
        command: str,
        *,
        user: str = "root",
        timeout: int = 30,
    ) -> CommandResult:
        return self._trace.capture(
            "run_command",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "command": command,
                "user": user,
                "timeout": timeout,
            },
            lambda: self._wrapped.run_command(command, user=user, timeout=timeout),
        )

    def write_file(self, path: str, content: str, *, user: str = "root") -> None:
        return self._trace.capture(
            "write_file",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "path": path,
                "user": user,
                **_content_summary(content),
            },
            lambda: self._wrapped.write_file(path, content, user=user),
            output=lambda _: {"written": True},
        )

    def read_file(self, path: str, *, user: str = "root") -> str:
        return self._trace.capture(
            "read_file",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "path": path,
                "user": user,
            },
            lambda: self._wrapped.read_file(path, user=user),
            output=_content_summary,
        )

    def list_files(self, path: str) -> list[dict[str, Any]]:
        return self._trace.capture(
            "list_files",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "path": path},
            lambda: self._wrapped.list_files(path),
        )

    def stat_file(self, path: str) -> dict[str, Any]:
        return self._trace.capture(
            "stat_file",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "path": path},
            lambda: self._wrapped.stat_file(path),
        )

    def file_exists(self, path: str) -> bool:
        return self._trace.capture(
            "file_exists",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "path": path},
            lambda: self._wrapped.file_exists(path),
        )

    def remove_file(self, path: str) -> None:
        return self._trace.capture(
            "remove_file",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "path": path},
            lambda: self._wrapped.remove_file(path),
        )

    def rename_file(self, old_path: str, new_path: str) -> dict[str, Any]:
        return self._trace.capture(
            "rename_file",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "old_path": old_path,
                "new_path": new_path,
            },
            lambda: self._wrapped.rename_file(old_path, new_path),
        )

    def make_dir(self, path: str) -> None:
        return self._trace.capture(
            "make_dir",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "path": path},
            lambda: self._wrapped.make_dir(path),
        )

    def write_files(self, files: list[tuple[str, str | bytes]]) -> int:
        return self._trace.capture(
            "write_files",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "files": [
                    {"path": path, **_content_summary(content)}
                    for path, content in files
                ],
            },
            lambda: self._wrapped.write_files(files),
            output=lambda written: {"written": written},
        )

    def watch_dir_events(
        self,
        path: str,
        operation: Callable[[], None],
        *,
        timeout: float = 5,
        until: Callable[[list[dict[str, str]]], bool] | None = None,
    ) -> list[dict[str, str]]:
        return self._trace.capture(
            "watch_dir_events",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "path": path,
                "timeout": timeout,
            },
            lambda: self._wrapped.watch_dir_events(
                path,
                operation,
                timeout=timeout,
                until=until,
            ),
        )

    def run_code(self, code: str, *, timeout: int = 60) -> CodeResult:
        return self._trace.capture(
            "run_code",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "code": code,
                "timeout": timeout,
            },
            lambda: self._wrapped.run_code(code, timeout=timeout),
        )

    def pause(self, *, timeout: int = 60) -> None:
        return self._trace.capture(
            "pause",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "timeout": timeout,
            },
            lambda: self._wrapped.pause(timeout=timeout),
            output=lambda _: {"paused": True},
        )

    def resume_or_connect(self, *, timeout: int = 60) -> SandboxAdapter:
        resumed = self._trace.capture(
            "resume_or_connect",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "timeout": timeout,
            },
            lambda: self._wrapped.resume_or_connect(timeout=timeout),
            output=lambda result: {"sandbox_id": result.sandbox_id},
        )
        return wrap_adapter(resumed, self._trace)

    def set_timeout(self, timeout: int) -> None:
        return self._trace.capture(
            "set_timeout",
            {"backend": self.backend, "sandbox_id": self.sandbox_id, "timeout": timeout},
            lambda: self._wrapped.set_timeout(timeout),
        )

    def create_snapshot(self) -> str:
        return self._trace.capture(
            "create_snapshot",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
            },
            self._wrapped.create_snapshot,
            output=lambda snapshot_id: {"snapshot_id": snapshot_id},
        )

    def delete_snapshot(self, snapshot_id: str) -> None:
        return self._trace.capture(
            "delete_snapshot",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "snapshot_id": snapshot_id,
            },
            lambda: self._wrapped.delete_snapshot(snapshot_id),
            output=lambda _: {"deleted": True},
        )

    def rollback(self, snapshot_id: str) -> dict[str, Any]:
        return self._trace.capture(
            "rollback",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "snapshot_id": snapshot_id,
            },
            lambda: self._wrapped.rollback(snapshot_id),
        )

    def clone(self, n: int = 1, *, concurrency: int = 1) -> list[SandboxAdapter]:
        clones = self._trace.capture(
            "clone",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "count": n,
                "concurrency": concurrency,
            },
            lambda: self._wrapped.clone(n=n, concurrency=concurrency),
            output=lambda results: {
                "sandbox_ids": [result.sandbox_id for result in results],
            },
        )
        return [wrap_adapter(clone, self._trace) for clone in clones]

    def list_snapshot_ids(self) -> set[str]:
        return self._trace.capture(
            "list_snapshot_ids",
            {"backend": self.backend, "sandbox_id": self.sandbox_id},
            self._wrapped.list_snapshot_ids,
        )

    def get_host(self, port: int) -> str:
        return self._trace.capture(
            "get_host",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                "port": port,
            },
            lambda: self._wrapped.get_host(port),
        )

    def traffic_access_token(self) -> str | None:
        return self._trace.capture(
            "traffic_access_token",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
            },
            self._wrapped.traffic_access_token,
            output=lambda token: {"token_present": bool(token)},
        )

    def update_network(self, network: dict | None = None) -> None:
        return self._trace.capture(
            "update_network",
            {
                "backend": self.backend,
                "sandbox_id": self.sandbox_id,
                # The policy itself, not just its keys: when a case asserts that
                # a connection was or was not torn down, the trace has to show
                # which policy caused it.
                "network": network,
            },
            lambda: self._wrapped.update_network(network),
            output=lambda _: {"updated": True},
        )

    def kill(self) -> None:
        return self._trace.capture(
            "kill",
            {"backend": self.backend, "sandbox_id": self.sandbox_id},
            self._wrapped.kill,
            output=lambda _: {"killed": True},
        )

    def close(self) -> None:
        return self._trace.capture(
            "close",
            {"backend": self.backend, "sandbox_id": self.sandbox_id},
            self._wrapped.close,
            output=lambda _: {"closed": True},
        )


def wrap_adapter(adapter: SandboxAdapter, trace: TraceCollector | None) -> SandboxAdapter:
    if trace is None or isinstance(adapter, TracingSandboxAdapter):
        return adapter
    return TracingSandboxAdapter(adapter, trace)


def _content_summary(content: Any) -> dict[str, Any]:
    text = str(content)
    return {
        "content_length": len(text),
    }
