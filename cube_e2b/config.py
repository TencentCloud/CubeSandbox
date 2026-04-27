"""
cube_e2b.config
~~~~~~~~~~~~~~~
Read configuration from environment variables.
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field


@dataclass
class SandboxConfig:
    """All runtime configuration for the cube_e2b SDK.

    Priority: constructor kwargs > environment variables > defaults.
    """

    # CubeAPI server address (E2B-compatible REST API)
    api_url: str = field(default_factory=lambda: os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))

    # API key — any non-empty string works for local CubeSandbox deployments
    api_key: str = field(default_factory=lambda: os.environ.get("E2B_API_KEY", "dummy"))

    # Default template ID used when Sandbox.create() is called without template=
    default_template_id: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_TEMPLATE_ID")
    )

    # If set, bypass *.cube.app DNS and connect directly to this IP for data streams.
    # e.g. CUBE_PROXY_NODE_IP=9.135.79.34
    proxy_node_ip: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_PROXY_NODE_IP")
    )

    # CubeProxy HTTP port (default 80)
    proxy_port_http: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
    )

    # CubeProxy HTTPS port (default 443)
    proxy_port_https: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTPS", "443"))
    )

    # Path to CA cert file (mkcert rootCA.pem) for self-signed HTTPS
    ssl_cert_file: str | None = field(
        default_factory=lambda: os.environ.get("SSL_CERT_FILE")
    )

    # Default sandbox timeout in seconds
    default_timeout: int = 300

    def __post_init__(self) -> None:
        self.api_url = self.api_url.rstrip("/")


# Module-level singleton — callers may instantiate their own or use this.
_default_config: SandboxConfig | None = None


def get_default_config() -> SandboxConfig:
    """Return (and lazily create) the module-level default :class:`SandboxConfig`."""
    global _default_config
    if _default_config is None:
        _default_config = SandboxConfig()
    return _default_config
