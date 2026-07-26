# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations


class CubeSandboxError(Exception):
    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code


class SandboxNotFoundError(CubeSandboxError): ...
class TemplateNotFoundError(CubeSandboxError): ...
class VolumeNotFoundError(CubeSandboxError): ...
class AuthenticationError(CubeSandboxError): ...
class ApiError(CubeSandboxError): ...
class FilesystemNotFoundError(CubeSandboxError): ...


class PartialWriteError(IOError):
    """Raised when write_files fails partway through.

    Attributes:
        written: Number of files successfully written before the failure.
    """

    def __init__(self, message: str, *, written: int):
        super().__init__(message)
        self.written = written
