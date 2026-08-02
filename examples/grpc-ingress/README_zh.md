# gRPC 接入快速示例

[English](README.md)

演示如何通过 CubeProxy **明文 gRPC 接入端口** `9090` 访问沙箱内服务。

CubeSandbox Python SDK 的 `commands`、`files` 等 API 走的是 CubeProxy
HTTP/HTTPS (`80`/`443`, 或 `CUBE_PROXY_NODE_IP` + `CUBE_PROXY_PORT_HTTP` 配 `Host`
头) 上的 **HTTP/Connect**。**这些 API 不需要改 SDK**。

当你使用 **原生 gRPC 客户端** (`grpcio`、`grpc-go` 等), 且无法使用泛域名
`*.cube.app` 或在 CubeProxy 上终止 TLS 时, 使用 `9090` 端口。

> **安全提示：** `9090` 是**明文** gRPC（示例使用 `grpc.insecure_channel`）。
> 不要直接暴露到不可信网络，除非在上游另有 TLS 终结（如负载均衡）。
> 需要 TLS 时，优先走 CubeProxy 的 HTTP/HTTPS（Connect 风格流量）。

## 流程

```text
  grpc_plaintext.py
        │
        ├─ Sandbox.create()  ──► CubeAPI (控制面)
        │
        └─ grpc.insecure_channel(proxy:9090, authority=<port>-<sandbox_id>)
                                    │
                                    ▼
                               CubeProxy :9090
                                    │
                                    ▼
                               沙箱内 envd (:49983)
```

## 前置条件

- 已部署 Cube Sandbox, 且 CubeProxy 已开启 gRPC 接入
- Python 3.8+
- 模板暴露 envd 端口 `49983` (标准 Cube 模板即可)

```bash
pip install -r requirements.txt
cp .env.example .env
# 编辑 .env
python grpc_plaintext.py
```

## 环境变量

| 变量 | 必填 | 默认 | 说明 |
|------|------|------|------|
| `CUBE_API_URL` | 是 | — | Cube API 地址 |
| `CUBE_TEMPLATE_ID` | 是 | — | 沙箱模板 ID |
| `CUBE_PROXY_NODE_IP` | 是 | — | CubeProxy IP (无需 DNS) |
| `CUBE_PROXY_GRPC_PORT` | 否 | `9090` | 明文 gRPC 监听端口 |
| `ENVD_PORT` | 否 | `49983` | `:authority` 中的沙箱服务端口 |

## 限制公网访问的沙箱

若创建沙箱时设置 `network={"allow_public_traffic": False}`, 每次 RPC 需在
gRPC metadata 中携带 token:

```python
metadata = (("cube-traffic-access-token", sandbox.traffic_access_token),)
# stub.SomeMethod(request, metadata=metadata)
```

详见[限制公网访问](../../docs/zh/guide/restrict-public-access.md)。

## 模板

```bash
cubemastercli tpl create-from-image \
  --image cubesandbox-base:latest \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```
