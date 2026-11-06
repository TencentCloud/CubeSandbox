# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations


class CubeSandboxError(Exception):
    def __init__(self, message: str, status_code: int | None = None, ret_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code
        self.ret_code = ret_code


class SandboxNotFoundError(CubeSandboxError): ...
class TemplateNotFoundError(CubeSandboxError): ...
class ApiError(CubeSandboxError): ...
class VolumeException(ApiError): ...
class NotFoundException(CubeSandboxError): ...
class VolumeNotFoundException(VolumeException, NotFoundException): ...
class VolumePathNotFoundException(VolumeException, NotFoundException): ...
class VolumeContentUnavailableError(VolumeException): ...

VolumeNotFoundError = VolumeNotFoundException
VolumePathNotFoundError = VolumePathNotFoundException
class AuthenticationError(CubeSandboxError): ...
AuthenticationException = AuthenticationError
class InvalidArgumentException(CubeSandboxError, ValueError): ...
class RateLimitException(CubeSandboxError):
    def __init__(self, message: str):
        super().__init__(message, status_code=429)

class ServiceBusyException(CubeSandboxError):
    def __init__(self, message: str):
        super().__init__(message, status_code=503)

class FilesystemNotFoundError(CubeSandboxError): ...


class PartialWriteError(IOError):
    """Raised when write_files fails partway through.

    Attributes:
        written: Number of files successfully written before the failure.
    """

    def __init__(self, message: str, *, written: int):
        super().__init__(message)
        self.written = written
