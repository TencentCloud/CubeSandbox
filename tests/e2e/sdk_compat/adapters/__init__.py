# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from adapters.base import SandboxAdapter
from adapters.cubesandbox_adapter import CubeSandboxAdapter
from adapters.e2b_adapter import E2BAdapter
from adapters.tracing_adapter import wrap_adapter
from framework.config import SdkE2EConfig
from framework.trace import get_current_trace, summarize_create_options

_ADAPTERS = {
    "cubesandbox": CubeSandboxAdapter,
    "e2b": E2BAdapter,
}


def create_adapter(
    backend: str,
    config: SdkE2EConfig,
    *,
    metadata: dict[str, str] | None = None,
    create_options: dict | None = None,
) -> SandboxAdapter:
    trace = get_current_trace()

    def _create() -> SandboxAdapter:
        return _adapter_for(backend).create(
            config,
            metadata=metadata,
            create_options=create_options,
        )

    if trace is None:
        return _create()
    adapter = trace.capture(
        "create",
        {
            "backend": backend,
            "template_id": config.cube_template_id,
            "metadata_keys": sorted((metadata or {}).keys()),
            "create_options": summarize_create_options(create_options),
        },
        _create,
        output=lambda result: {"sandbox_id": result.sandbox_id},
    )
    return wrap_adapter(adapter, trace)


def connect_adapter(backend: str, sandbox_id: str, config: SdkE2EConfig) -> SandboxAdapter:
    trace = get_current_trace()

    def _connect() -> SandboxAdapter:
        return _adapter_for(backend).connect(sandbox_id, config)

    if trace is None:
        return _connect()
    adapter = trace.capture(
        "connect",
        {"backend": backend, "sandbox_id": sandbox_id},
        _connect,
        output=lambda result: {"sandbox_id": result.sandbox_id},
    )
    return wrap_adapter(adapter, trace)


def list_sandboxes(backend: str, config: SdkE2EConfig) -> list[dict]:
    trace = get_current_trace()

    def _list() -> list[dict]:
        return _adapter_for(backend).list_sandboxes(config)

    if trace is None:
        return _list()
    return trace.capture(
        "list_sandboxes",
        {"backend": backend},
        _list,
        output=lambda entries: {
            "count": len(entries),
            "sandboxes": [
                {
                    "sandbox_id": _entry_id(entry),
                    "state": entry.get("state"),
                }
                for entry in entries
                if isinstance(entry, dict)
            ],
        },
    )


def _adapter_for(backend: str):
    try:
        return _ADAPTERS[backend]
    except KeyError as exc:
        raise ValueError(f"unknown SDK E2E backend: {backend}") from exc


def _entry_id(entry: dict):
    if "sandboxID" in entry:
        return entry["sandboxID"]
    return entry.get("sandbox_id")


__all__ = ["SandboxAdapter", "connect_adapter", "create_adapter", "list_sandboxes"]
