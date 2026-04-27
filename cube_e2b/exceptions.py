"""
cube_e2b.exceptions
~~~~~~~~~~~~~~~~~~~~
Exception hierarchy for the cube_e2b SDK.
"""
from __future__ import annotations


class CubeSandboxError(Exception):
    """Base exception for all cube_e2b errors."""

    def __init__(self, message: str, status_code: int | None = None) -> None:
        super().__init__(message)
        self.status_code = status_code


class SandboxNotFoundError(CubeSandboxError):
    """Raised when the requested sandbox does not exist (HTTP 404)."""


class SandboxTimeoutError(CubeSandboxError):
    """Raised when a sandbox operation times out."""


class TemplateNotFoundError(CubeSandboxError):
    """Raised when the requested template does not exist."""


class AuthenticationError(CubeSandboxError):
    """Raised on HTTP 401 / 403."""


class ApiError(CubeSandboxError):
    """Generic API error with an HTTP status code."""
