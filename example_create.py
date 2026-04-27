#!/usr/bin/env python3
"""
example_create.py
-----------------
最简单的验证脚本：创建 sandbox、获取信息、销毁。

用法：
    # 在 CubeSandbox 服务器上（DNS 自动解析）：
    export CUBE_API_URL=http://127.0.0.1:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    python example_create.py

    # 在远程客户端（绕过 DNS）：
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python example_create.py
"""
import os
import sys

# 也可以直接在代码里设置，优先级低于已有环境变量
os.environ.setdefault("CUBE_API_URL", "http://9.135.79.34:3000")
os.environ.setdefault("CUBE_TEMPLATE_ID", "tpl-6265796cee124256b4dcd6a1")
os.environ.setdefault("CUBE_PROXY_NODE_IP", "9.135.79.34")

from cube_e2b import Sandbox

def main():
    print("=== Creating sandbox ===")
    sb = Sandbox.create(timeout=120)
    print(f"  sandbox_id : {sb.sandbox_id}")
    print(f"  template   : {sb.template_id}")
    print(f"  domain     : {sb.domain}")
    print(f"  host:49999 : {sb.get_host(49999)}")
    print(f"  url:49999  : {sb.get_url(49999)}")

    print("\n=== HTTP GET /  (port 49999) ===")
    try:
        resp = sb.http_get(49999, "/")
        print(f"  status : {resp.status_code}")
        print(f"  body   : {resp.text[:200]}")
    except Exception as e:
        print(f"  [expected 404 from CubeProxy] {e}")

    print("\n=== Sandbox info ===")
    info = sb.info()
    print(f"  state : {info.get('state', 'N/A')}")

    print("\n=== Killing sandbox ===")
    sb.kill()
    print("  done.")


if __name__ == "__main__":
    main()
