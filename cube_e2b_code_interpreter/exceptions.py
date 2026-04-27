"""
cube_e2b_code_interpreter.exceptions
"""
from __future__ import annotations


class CubeCodeInterpreterError(Exception):
    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code

class SandboxNotFoundError(CubeCodeInterpreterError): pass
class TemplateNotFoundError(CubeCodeInterpreterError): pass
class AuthenticationError(CubeCodeInterpreterError): pass
class TimeoutError(CubeCodeInterpreterError): pass
class ApiError(CubeCodeInterpreterError): pass
