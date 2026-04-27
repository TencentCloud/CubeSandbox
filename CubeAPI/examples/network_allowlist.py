# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_allowlist.py — 只允许访问指定 IP/CIDR，拦截其余所有出口流量

使用场景：
    沙箱需要访问特定内部服务（数据库、对象存储、内部 API），
    同时禁止访问其他地址，防止数据渗漏。

原理：
    network.allow_out 设置白名单 CIDR 列表，传入 CubeVSContext.AllowOut，
    Cubelet tap 网络层只允许匹配的目标地址通过。
"""

import os
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

template_id = os.environ["CUBE_TEMPLATE_ID"]

# 只允许访问内部 DNS（10.0.0.53）和对象存储段（10.0.1.0/24）
ALLOWED_CIDRS = [
    "10.0.0.53/32",   # 内部 DNS
    "10.0.1.0/24",    # 内部对象存储网段
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={
        "allow_out": ALLOWED_CIDRS,
    },
) as sandbox:
    # 白名单内地址可达
    result = sandbox.commands.run(
        "curl -s --max-time 3 http://10.0.0.53 -o /dev/null -w '%{http_code}' || echo 'unreachable'"
    )
    print("internal DNS reachable:", result.stdout.strip())

    # 白名单外地址被拦截
    result = sandbox.commands.run(
        "curl -s --max-time 3 https://8.8.8.8 -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("external DNS blocked:", result.stdout.strip())

    result = sandbox.commands.run("echo 'allowlist network ok'")
    print(result.stdout.strip())
