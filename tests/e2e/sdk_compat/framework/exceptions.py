# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations


class SdkCompatError(Exception):
    """Base exception for SDK compatibility E2E helpers."""


class UnsupportedCapability(SdkCompatError):
    """Raised when a backend cannot express a requested capability."""

    def __init__(self, backend: str, capability: str) -> None:
        super().__init__(f"backend {backend!r} does not support capability {capability!r}")
        self.backend = backend
        self.capability = capability
