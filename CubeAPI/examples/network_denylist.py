# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_denylist.py — 允许访问公网，但拦截特定 IP/CIDR

使用场景：
    沙箱需要正常上网（安装包、拉取资源），
    但要屏蔽云厂商 metadata 接口、内网敏感段等特定地址，
    防止沙箱内代码探测宿主机信息或横向移动。

原理：
    network.deny_out 设置黑名单 CIDR 列表，传入 CubeVSContext.DenyOut，
    Cubelet tap 网络层丢弃所有目标地址匹配的出口流量。
"""

import os
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

template_id = os.environ["CUBE_TEMPLATE_ID"]

# 屏蔽云厂商 metadata 接口和内网管理段
DENIED_CIDRS = [
    "169.254.0.0/16",  # link-local（AWS/GCP/腾讯云 metadata 通用段）
    "100.100.100.200/32",  # 阿里云 metadata
    "10.0.0.0/8",     # 内网管理段
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=True,
    network={
        "deny_out": DENIED_CIDRS,
    },
) as sandbox:
    # 公网仍可正常访问
    result = sandbox.commands.run(
        "curl -s --max-time 5 https://example.com -o /dev/null -w '%{http_code}'"
    )
    print("public internet:", result.stdout.strip())

    # metadata 接口被拦截
    result = sandbox.commands.run(
        "curl -s --max-time 3 http://169.254.169.254/latest/meta-data/ || echo 'blocked'"
    )
    print("metadata endpoint blocked:", "blocked" in result.stdout or result.exit_code != 0)

    result = sandbox.commands.run("echo 'denylist network ok'")
    print(result.stdout.strip())
