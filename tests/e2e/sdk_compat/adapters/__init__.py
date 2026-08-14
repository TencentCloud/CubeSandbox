# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from collections.abc import Callable

from adapters.base import SandboxAdapter
from adapters.cubesandbox_adapter import CubeSandboxAdapter
from adapters.e2b_adapter import E2BAdapter
from adapters.tracing_adapter import wrap_adapter
from framework.config import SdkE2EConfig
from framework.create_retry import create_with_capacity_retry
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


def create_adapter_with_capacity_retry(
    backend: str,
    config: SdkE2EConfig,
    *,
    metadata: dict[str, str] | None = None,
    create_options: dict | None = None,
    on_retry: Callable[[int, float, BaseException], None] | None = None,
) -> SandboxAdapter:
    """``create_adapter`` that retries while the scheduler is out of capacity.

    Use this at success-expecting create sites so a saturated shared CI node can
    be reclaimed instead of failing the run. Do NOT use it where a create is
    expected to be rejected (e.g. boundary/rejection tests).
    """
    return create_with_capacity_retry(
        lambda: create_adapter(
            backend,
            config,
            metadata=metadata,
            create_options=create_options,
        ),
        retries=config.create_capacity_retries,
        backoff=config.create_capacity_backoff,
        backoff_max=config.create_capacity_backoff_max,
        total_budget=config.create_capacity_budget,
        on_retry=on_retry,
    )


def connect_adapter(
    backend: str,
    sandbox_id: str,
    config: SdkE2EConfig,
    *,
    timeout: int | None = None,
) -> SandboxAdapter:
    trace = get_current_trace()

    def _connect() -> SandboxAdapter:
        return _adapter_for(backend).connect(sandbox_id, config, timeout=timeout)

    if trace is None:
        return _connect()
    adapter = trace.capture(
        "connect",
        {"backend": backend, "sandbox_id": sandbox_id, "timeout": timeout},
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


__all__ = [
    "SandboxAdapter",
    "connect_adapter",
    "create_adapter",
    "create_adapter_with_capacity_retry",
    "list_sandboxes",
]
