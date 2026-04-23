[English](./README.md) | [中文](./README_zh.md)

# E2B Dev Sidecar

这个 demo 展示一件事：本地直接复用 `e2b_code_interpreter` SDK 访问 CubeSandbox，同时把沙箱数据面流量转发到 CubeProxy。

## 为什么需要dev-sidecar?

由于 E2B 需要把沙箱 URL 通过泛解析解析到目标集群的 public IP，因此如果你是在生产环境中部署，通常需要在私有 DNS 中添加一条 A 记录：

```text
*.cube.app => <your cube master node ip>
```

但在开发阶段，在自己电脑上配置泛解析通常很麻烦。`dev-sidecar` 的作用，就是帮助你在不修改 E2B SDK 的情况下，直接从开发机连接 Cube 集群并创建实例。

适用场景：

- 控制面已经能通过 `E2B_API_URL` 访问 CubeAPI
- 数据面需要经过一个本地 sidecar 改写 `Host` 后再转发到 CubeProxy

## 文件

- `demo.py`：最小可运行示例
- `dev_sidecar.py`：启动内嵌 sidecar，并把 SDK 的数据面访问改写到 sidecar
- `env.example`：示例环境变量

## 快速开始

```bash
cd examples/e2b-dev-sidecar
pip install -r requirements.txt
cp env.example .env
```

编辑 `.env`，至少填这三个值：

```bash
# **If you are running Cube on remote machine,** replace this with:  http://<node-ip>:3000
E2B_API_URL="http://127.0.0.1:13000"
# **If you are running Cube on remote machine,** replace this with:  https://<node-ip>:443
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="<your-template-id>"
```

如果目标集群开启了鉴权，运行前要把 `.env` 里的 `E2B_API_KEY="dummy"` 换成真实 API key。

然后运行：

```bash
python demo.py
```

成功时会输出类似：

```text
Hello world Cube！
```

## 这个 demo 做了什么

`demo.py` 启动时会先调用 `setup_dev_sidecar()`，它会做两件事：

1. 在本地启动一个 sidecar；默认监听 `127.0.0.1:12580`，端口被占用时自动换下一个可用端口。
2. monkey patch 那些确实需要经过数据面的 SDK helper。sidecar 模式下，envd、文件 URL、MCP URL 和沙箱端口访问都会改为请求：

```text
http://127.0.0.1:<local-port>/sandboxes/router/<sandbox_id>/<port>
```

sidecar 再把这类请求转发到 `CUBE_REMOTE_PROXY_BASE`，并把 `Host` 改写为：

```text
<port>-<sandbox_id>.<sandbox-domain>
```

这包括：

- envd API 流量
- 文件上传/下载 URL
- MCP URL
- 普通沙箱 HTTP 流量
- 通过同一路由转发的 WebSocket 流量

## 配置说明

- `E2B_API_URL`
  控制面地址，SDK 会直接请求 CubeAPI，不经过 sidecar。
- `CUBE_REMOTE_PROXY_BASE`
  sidecar 转发数据面请求时使用的 CubeProxy 地址。
- `CUBE_TEMPLATE_ID`
  创建 sandbox 时使用的模板 ID。
- `CUBE_REMOTE_SANDBOX_DOMAIN`
  可选，默认 `cube.app`。
- `CUBE_REMOTE_PROXY_VERIFY_SSL`
  可选，默认 `false`，方便自签证书或本地开发环境。
- `CUBE_DEV_PROXY_HOST`
  可选，内嵌 sidecar 的监听地址，默认 `127.0.0.1`。
- `CUBE_DEV_PROXY_PORT`
  可选，内嵌 sidecar 的首选端口，默认 `12580`。
- `CUBE_DEV_PROXY_URL`
  可选。如果你已经有外部 sidecar，可以直接指向它；此时不会再启动内嵌 sidecar。

## Sidecar URL 语义

- sidecar 模式下，`sandbox.get_host(port)` 返回的是 host 加 router path 片段，不是纯 DNS host。
- `download_url()`、`upload_url()`、`get_mcp_url()` 返回的是完整 routed URL，并保留 sidecar 自己的 scheme。
- 内嵌 sidecar 默认始终监听在 `http://...`。
- 如果使用 `CUBE_DEV_PROXY_URL`，生成出来的文件 URL 和 MCP URL 会跟随这个 URL 的 scheme。

## 开发边界

- 这个 demo 只代理数据面，不代理控制面。
- 这个 sidecar 是 dev-only 的最小实现，不是生产网关。
