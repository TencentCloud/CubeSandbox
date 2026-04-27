#!/usr/bin/env python3
"""
example_stream.py
-----------------
演示通过 CUBE_PROXY_NODE_IP 绕过 DNS 进行：
  1. HTTP 流式读取（SSE / chunked）
  2. WebSocket 连接

用法：
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python example_stream.py
"""
import os

os.environ.setdefault("CUBE_API_URL", "http://9.135.79.34:3000")
os.environ.setdefault("CUBE_TEMPLATE_ID", "tpl-6265796cee124256b4dcd6a1")
os.environ.setdefault("CUBE_PROXY_NODE_IP", "9.135.79.34")

from cube_e2b import Sandbox
from cube_e2b.stream import build_stream_session

def demo_http_stream(sb: Sandbox):
    """Stream chunked HTTP response from sandbox port 49999."""
    print("\n=== HTTP stream (chunked) from port 49999 ===")
    try:
        session = build_stream_session()
        host = sb.get_host(49999)
        url = f"http://{host}/"
        print(f"  Connecting to: {url}")
        print(f"  (via proxy IP: {os.environ.get('CUBE_PROXY_NODE_IP')})")
        with session.get(url, stream=True, timeout=5) as resp:
            print(f"  Status: {resp.status_code}")
            for chunk in resp.iter_content(chunk_size=256):
                if chunk:
                    print(f"  Chunk: {chunk[:100]}")
                    break
    except Exception as e:
        print(f"  [{type(e).__name__}] {e}")


def demo_websocket(sb: Sandbox):
    """Try WebSocket connection to sandbox port 49999."""
    print("\n=== WebSocket connect to port 49999 ===")
    try:
        print(f"  host: {sb.get_host(49999)}")
        with sb.connect_ws(49999, "/ws") as ws:
            print("  Connected!")
            ws.send("ping")
            msg = ws.recv(timeout=3)
            print(f"  Received: {msg}")
    except ImportError as e:
        print(f"  [Skip] {e}")
    except Exception as e:
        print(f"  [{type(e).__name__}] {e}")


def main():
    print("=== Creating sandbox ===")
    with Sandbox.create(timeout=60) as sb:
        print(f"  id: {sb.sandbox_id}")
        print(f"  CUBE_PROXY_NODE_IP: {os.environ.get('CUBE_PROXY_NODE_IP', 'not set')}")

        demo_http_stream(sb)
        demo_websocket(sb)

        print("\n=== Done, sandbox auto-killed on exit ===")


if __name__ == "__main__":
    main()
