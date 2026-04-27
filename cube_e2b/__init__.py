"""
cube_e2b
~~~~~~~~
Python SDK for CubeSandbox — E2B-compatible sandbox service.

Quick start::

    import os
    os.environ["CUBE_API_URL"] = "http://9.135.79.34:3000"
    os.environ["CUBE_TEMPLATE_ID"] = "tpl-6265796cee124256b4dcd6a1"
    os.environ["CUBE_PROXY_NODE_IP"] = "9.135.79.34"   # bypass *.cube.app DNS

    from cube_e2b import Sandbox

    with Sandbox.create() as sb:
        resp = sb.http_get(49999, "/health")
        print(resp.text)
"""
from .config import SandboxConfig, get_default_config
from .exceptions import (
    ApiError,
    AuthenticationError,
    CubeSandboxError,
    SandboxNotFoundError,
    SandboxTimeoutError,
    TemplateNotFoundError,
)
from .sandbox import Sandbox

__all__ = [
    "Sandbox",
    "SandboxConfig",
    "get_default_config",
    "CubeSandboxError",
    "SandboxNotFoundError",
    "SandboxTimeoutError",
    "TemplateNotFoundError",
    "AuthenticationError",
    "ApiError",
]

__version__ = "0.1.0"
