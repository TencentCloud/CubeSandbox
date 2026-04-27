"""
cube_e2b_code_interpreter.config
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
环境变量读取，完全对齐 e2b_code_interpreter 的 ConnectionConfig。
"""
from __future__ import annotations
import os
from dataclasses import dataclass, field


@dataclass
class SandboxConfig:
    """
    环境变量优先级：构造函数参数 > 环境变量 > 默认值

    对比 e2b_code_interpreter ConnectionConfig：
        E2B_API_KEY       → api_key        (必须非空，本地随意填)
        E2B_API_URL       → api_url        (e2b cloud 用不到，本地必须设)
        E2B_DOMAIN        → 无对应         (e2b 用 *.e2b.app，cube 用 *.cube.app)

    新增 CubeSandbox 特有：
        CUBE_API_URL          → api_url        (CubeAPI REST 地址)
        CUBE_TEMPLATE_ID      → template_id    (默认模板)
        CUBE_PROXY_NODE_IP    → proxy_node_ip  (绕过 DNS 直连代理 IP)
        CUBE_PROXY_PORT_HTTP  → proxy_port_http
        CUBE_PROXY_PORT_HTTPS → proxy_port_https
        CUBE_SANDBOX_DOMAIN   → sandbox_domain (默认 cube.app)
        SSL_CERT_FILE         → ssl_cert_file
    """

    api_url: str = field(
        default_factory=lambda: os.environ.get("CUBE_API_URL",
                                os.environ.get("E2B_API_URL", "http://127.0.0.1:3000"))
    )
    # E2B_API_KEY 仅为兼容原版 e2b SDK 的非空校验保留
    # cube-api 本地部署不做鉴权，不设置此变量也完全正常
    api_key: str = field(
        default_factory=lambda: os.environ.get("E2B_API_KEY", "dummy")
    )
    template_id: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_TEMPLATE_ID")
    )

    # ---- DNS 绕过 ----
    # 原版 e2b：不需要，*.e2b.app 由 e2b cloud DNS 解析
    # CubeSandbox：CoreDNS 只在本机监听，其他机器需要直连 IP
    proxy_node_ip: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_PROXY_NODE_IP")
    )
    proxy_port_http: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
    )
    proxy_port_https: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTPS", "443"))
    )

    # Sandbox 服务域名，e2b 用 e2b.app，CubeSandbox 用 cube.app
    sandbox_domain: str = field(
        default_factory=lambda: os.environ.get("CUBE_SANDBOX_DOMAIN", "cube.app")
    )

    # mkcert 自签名 CA
    ssl_cert_file: str | None = field(
        default_factory=lambda: os.environ.get("SSL_CERT_FILE")
    )

    default_timeout: int = 300
    request_timeout: float = 30.0

    def __post_init__(self):
        self.api_url = self.api_url.rstrip("/")
