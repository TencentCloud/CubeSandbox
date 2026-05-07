# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
from dataclasses import dataclass, field


@dataclass
class Config:
    """SDK configuration. All fields can be set via environment variables."""

    api_url: str = field(
        default_factory=lambda: os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
    )
    template_id: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_TEMPLATE_ID")
    )
    proxy_node_ip: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_PROXY_NODE_IP")
    )
    proxy_port: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
    )
    sandbox_domain: str = field(
        default_factory=lambda: os.environ.get("CUBE_SANDBOX_DOMAIN", "cube.app")
    )
    timeout: int = 300
    request_timeout: float = 30.0

    def __post_init__(self) -> None:
        self.api_url = self.api_url.rstrip("/")
