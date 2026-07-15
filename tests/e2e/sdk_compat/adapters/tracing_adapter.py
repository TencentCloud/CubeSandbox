# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

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
