# mcp-gateway 示例

[English](README.md)

这是为与 E2B 兼容的 `mcp` 沙箱参数提供的一个 `mcp-gateway` 参考实现，并附带一个把它打成沙箱模板的 Dockerfile。SDK 在沙箱内只会执行 `mcp-gateway --config '<json>'`，任何满足[网关约定](../../docs/zh/guide/mcp-gateway.md#网关约定)的程序都可以替换它。提供这个示例，是为了让你能端到端地试用该功能，并在此基础上改造自己的模板。

它以 stdio 方式启动所需的 MCP 服务，并将它们的全部工具聚合到端口 `50005` 上一个带 Bearer 鉴权的 streamable HTTP 端点（`/mcp`）。使用说明见 [docs/zh/guide/mcp-gateway.md](../../docs/zh/guide/mcp-gateway.md)。

## 构建模板

```bash
cd examples/mcp-gateway
docker build -f template/Dockerfile \
  --build-arg BASE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --build-arg MCP_SERVERS="duckduckgo fetch time arxiv" \
  -t <镜像仓库>/mcp-gateway:latest .
docker push <镜像仓库>/mcp-gateway:latest

cubemastercli tpl create-from-image \
  --image <镜像仓库>/mcp-gateway:latest \
  --alias mcp-gateway \
  --writable-layer-size 1G \
  --expose-port 49983 --expose-port 49999 --expose-port 50005 \
  --probe 49983 --probe-path /health
```

构建参数：

- `BASE_IMAGE`：基础镜像，默认是 CubeSandbox 的 `sandbox-code` 镜像，其中已包含 `uv`、`node` 和 `git`。
- `MCP_SERVERS`：需要预装的目录服务。要用到的服务请全部预装：SDK 最多等待网关 60 秒，未预装的服务会在首次启动时下载。

## 命令

| 命令 | 用途 |
| --- | --- |
| `mcp-gateway --config '<json>'` | 校验配置，在后台启动网关，并等待所有服务就绪（默认最多 55 秒，可用 `--startup-timeout` 调整）。SDK 在创建沙箱后执行的就是这条命令。 |
| `mcp-gateway serve` | 在前台运行网关。 |
| `mcp-gateway pull <server>...` | 构建模板时预装目录中的服务。 |
| `mcp-gateway catalog` | 列出内置服务及模板提供的服务。 |

启动器从 `GATEWAY_ACCESS_TOKEN` 读取 Token，未设置时自动生成。Token 写入 `/etc/mcp-gateway/.token`（权限 `0600`）；配置和 Token 通过 stdin 传给后台进程：配置不会落盘，两者也都不会出现在后台进程的参数或环境变量中。但在启动器退出之前，配置会出现在启动器自己的进程参数中，因为 SDK 就是这样传入配置的（`--config '<json>'`）。

所有 MCP 服务都以 `root` 身份运行，彼此之间没有隔离：任何一个服务都能读取其他服务的 `envs` 和 Token 文件。请把同一沙箱内的服务都当作可信服务，详见[安全说明](../../docs/zh/guide/mcp-gateway.md#安全说明)。

## 目录结构

- `cmd/mcp-gateway`：程序入口。
- `internal/cli`：启动器以及 `serve`、`pull`、`catalog` 子命令。
- `internal/config`：`mcp` 配置解析和内置目录（`catalog.json`）。模板可通过 `/etc/mcp-gateway/catalog.json` 扩展目录。
- `internal/gateway`：上游服务管理、工具聚合和 Bearer 鉴权，基于官方 [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) 实现。
- `template/Dockerfile`：用于构建 `mcp-gateway` 沙箱模板。

## 构建与测试

需要 Go 1.25 或更高版本。

```bash
cd examples/mcp-gateway
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o mcp-gateway ./cmd/mcp-gateway
```
