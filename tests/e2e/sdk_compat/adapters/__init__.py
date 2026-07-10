# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from framework.config import SdkE2EConfig

from adapters.base import SandboxAdapter
from adapters.cubesandbox_adapter import CubeSandboxAdapter
from adapters.e2b_adapter import E2BAdapter


def create_adapter(
    backend: str,
    config: SdkE2EConfig,
    *,
    metadata: dict[str, str] | None = None,
) -> SandboxAdapter:
    if backend == "cubesandbox":
        return CubeSandboxAdapter.create(config, metadata=metadata)
    if backend == "e2b":
        return E2BAdapter.create(config, metadata=metadata)
    raise ValueError(f"unknown SDK E2E backend: {backend}")


__all__ = ["SandboxAdapter", "create_adapter"]
