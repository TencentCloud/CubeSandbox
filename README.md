# cube_e2b — CubeSandbox Python SDK

轻量级 Python SDK，封装 CubeSandbox 的 E2B 兼容 REST API。

**核心特性：**
- 支持 `CUBE_PROXY_NODE_IP` 环境变量，**完全绕过 `*.cube.app` DNS**，直连代理 IP
- 兼容 E2B SDK 的 Sandbox 接口风格
- 支持 HTTP / SSE 数据流 + WebSocket

---

## 安装

```bash
pip install requests websockets
# 或
pip install -r requirements.txt
```

---

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CUBE_API_URL` | `http://127.0.0.1:3000` | CubeAPI 地址 |
| `E2B_API_KEY` | `dummy` | API Key（本地部署任意字符串） |
| `CUBE_TEMPLATE_ID` | — | 默认模板 ID |
| `CUBE_PROXY_NODE_IP` | — | **设置后绕过 DNS**，直连该 IP 访问数据流 |
| `CUBE_PROXY_PORT_HTTP` | `80` | CubeProxy HTTP 端口 |
| `CUBE_PROXY_PORT_HTTPS` | `443` | CubeProxy HTTPS 端口 |
| `SSL_CERT_FILE` | — | mkcert CA 证书路径（HTTPS 时用） |

---

## 快速开始

```python
import os
os.environ["CUBE_API_URL"]      = "http://9.135.79.34:3000"
os.environ["CUBE_TEMPLATE_ID"]  = "tpl-6265796cee124256b4dcd6a1"
os.environ["CUBE_PROXY_NODE_IP"] = "9.135.79.34"  # 绕过 *.cube.app DNS

from cube_e2b import Sandbox

with Sandbox.create() as sb:
    print(sb.sandbox_id)               # 5405bd0b3b584ac6bafb7656ebe19f8c
    print(sb.get_host(49999))          # 49999-xxx.cube.app
    resp = sb.http_get(49999, "/")     # 直连 9.135.79.34:80，Host 头自动设置
    print(resp.status_code)
```

---

## API 说明

### `Sandbox.create(...)`

```python
sb = Sandbox.create(
    template="tpl-xxxx",   # 模板 ID，可省略（读 CUBE_TEMPLATE_ID）
    timeout=300,            # TTL 秒数
    env_vars={"KEY": "val"},
)
```

### URL / Host

```python
sb.get_host(49999)          # "49999-<id>.cube.app"
sb.get_url(49999)           # "http://49999-<id>.cube.app"
sb.get_url(49999, "wss")    # "wss://49999-<id>.cube.app"
```

### HTTP 访问

```python
resp = sb.http_get(49999, "/api/v1/status")
resp = sb.http_post(49999, "/run", json={"code": "print(1)"})
```

### SSE 数据流

```python
for line in sb.iter_sse(49999, "/events"):
    print(line)
```

### WebSocket（需要 `websockets>=12`）

```python
with sb.connect_ws(49999, "/ws") as ws:
    ws.send("hello")
    print(ws.recv())
```

### 沙箱生命周期

```python
sb.refresh(600)   # 延长 TTL
sb.pause()        # 快照暂停
sb.resume(300)    # 恢复
sb.kill()         # 销毁
```

---

## DNS 绕过原理

当 `CUBE_PROXY_NODE_IP=9.135.79.34` 时：

```
客户端                          CubeProxy (9.135.79.34:80)
  │                                   │
  │  TCP connect → 9.135.79.34:80    │
  │  GET / HTTP/1.1                   │
  │  Host: 49999-xxx.cube.app  ──────▶│ 按 Host 路由到对应 sandbox
  │                                   │
```

`requests` 使用自定义 `HTTPAdapter` 将所有连接重定向到代理 IP，同时保留 `Host` 头，使 CubeProxy 能正确路由。WebSocket 同理，通过 `socket.create_connection` 直连 IP。

---

## 当前部署信息

| 字段 | 值 |
|------|----|
| 服务器 | `9.135.79.34` |
| API 端口 | `3000` |
| Proxy HTTP | `80` |
| Proxy HTTPS | `443` |
| 模板 ID | `tpl-6265796cee124256b4dcd6a1` |
| 域名 | `*.cube.app` → `9.135.79.34` |
