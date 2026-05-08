# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations


class CubeSandboxError(Exception):
    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code


class SandboxNotFoundError(CubeSandboxError): ...
class TemplateNotFoundError(CubeSandboxError): ...
class AuthenticationError(CubeSandboxError): ...
class ApiError(CubeSandboxError): ...
