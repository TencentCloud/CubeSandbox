# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_no_internet.py — 完全禁止沙箱访问公网

使用场景：
    代码执行、数据处理等不需要外网访问的任务，隔离沙箱防止数据外泄。

原理：
    allow_internet_access=False 会在 Cubelet 的 tap 网络层设置
    CubeVSContext.AllowInternetAccess=false，阻断所有公网出口流量。
"""

import os
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
) as sandbox:
    # 验证：公网不可达（curl 应超时或返回 connection refused）
    result = sandbox.commands.run(
        "curl -s --max-time 3 https://example.com -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("internet access blocked:", result.stdout.strip() == "blocked" or result.exit_code != 0)

    # 沙箱内部逻辑仍正常工作
    result = sandbox.commands.run("echo 'isolated execution ok'")
    print(result.stdout.strip())
