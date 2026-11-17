# 在沙箱中使用 MCP

::: warning 预览
MCP 支持目前处于预览阶段，`mcp` 参数和示例网关都可能调整。
:::

[MCP](https://modelcontextprotocol.io/)（Model Context Protocol）是 AI Agent 发现和调用工具的协议，工具可以是网页搜索、文件读写、论文检索等。每类工具由一个 **MCP 服务**（MCP server）提供，它通常是一个通过 stdio 通信的小程序。

与 E2B 兼容的 `mcp` 参数用于在创建沙箱时声明需要哪些 MCP 服务。这些服务运行在沙箱**内部**，与你自己的机器隔离。创建后你会拿到一个 URL 和一个 Token，Agent 连上这个 URL，就能看到并调用所有已声明服务提供的工具：

```python
from cubesandbox import Sandbox

sandbox = Sandbox.create(mcp={"time": {}, "fetch": {}})
url = sandbox.get_mcp_url()      # http://50005-<sandbox-id>.<domain>/mcp
token = sandbox.get_mcp_token()  # 以 "Authorization: Bearer <token>" 发送
```

同样的代码也可以用原生 E2B SDK 运行，只需将其指向 CubeAPI。

## 工作原理

```text
你的 Agent ──(MCP over HTTP + Token)──> 50005-<id>.<domain>/mcp
                                              │  CubeProxy
                                              ▼
                                      沙箱内的 mcp-gateway
                                       ├── time 服务   (stdio)
                                       └── fetch 服务  (stdio)
```

1. **CubeAPI**：校验 `mcp` 的结构，格式不对时在创建任何沙箱之前返回 `400`。它不保存这个值，也不会下发给调度层或在响应中返回。
2. **模板**：SDK 按[模板优先级](#模板优先级)中的顺序确定模板。**该模板中必须包含 `mcp-gateway` 程序**，默认沙箱镜像里没有。
3. **SDK**：沙箱启动后，SDK 以 `root` 身份执行 `mcp-gateway --config '<json>'`，并通过 `GATEWAY_ACCESS_TOKEN` 传入随机 Token，最多等待 60 秒，与 E2B SDK 一致。
4. **网关**：拉起所需的服务，并在端口 `50005` 上对外提供它们的工具。**CubeProxy** 会像转发其他沙箱端口一样，把 `50005-<id>.<domain>` 路由到这个端口。
5. **失败处理**：如果命令失败或超时，SDK 会销毁沙箱，并抛出附带网关输出的 `Failed to start MCP gateway: ...` 错误。

Cube 本身只负责第 1、3 步和路由；沙箱里实际跑什么，由模板决定。

## 模板优先级

带 `mcp` 创建沙箱时，Cube SDK 按以下顺序选择模板：

1. 创建沙箱时传了 `template`（Go 中为 `TemplateID`），使用对应的模板
2. 没有传 `template`，则使用别名为 `mcp-gateway` 的模板
3. 如果别名为 `mcp-gateway` 的模板不存在，则使用环境变量 `CUBE_TEMPLATE_ID` 对应的模板
4. 如果别名为 `mcp-gateway` 的模板不存在，且环境变量 `CUBE_TEMPLATE_ID` 对应的模板也不存在，则创建失败并报模板不存在的错误

> 如果希望修改 mcp 默认使用的模板别名（默认别名为 `mcp-gateway`），可以设置环境变量 `CUBE_MCP_TEMPLATE_ID`：
> ```bash
> export CUBE_MCP_TEMPLATE_ID=custom-mcp-gateway
> ```

## 网关约定

任何程序都可以充当网关，只要它以 `mcp-gateway` 为名装在 `PATH` 中，并满足以下约定：

| 项目 | 要求 |
| --- | --- |
| 命令 | 以 `root` 身份执行 `mcp-gateway --config '<json>'`，其中 `<json>` 是创建沙箱时传入的 `mcp` 对象。 |
| Token | 从环境变量 `GATEWAY_ACCESS_TOKEN` 读取，并写入 `/etc/mcp-gateway/.token`。`Sandbox.connect` 之后，`get_mcp_token()` 就是从这个文件取回 Token 的。 |
| 就绪 | 命令退出后网关需继续提供服务；只有端点就绪后才以 `0` 退出，失败时以非零码退出并输出可读的错误信息。必须在 60 秒内完成。 |
| 端点 | 端口 `50005`、路径 `/mcp` 上的 MCP streamable HTTP 端点，拒绝不带 `Authorization: Bearer <token>` 的请求。 |
| 模板 | 创建模板时暴露端口 `50005`。 |

## 使用示例网关快速上手

仓库在 [`examples/mcp-gateway`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mcp-gateway) 中提供了一个参考实现：它拉起 stdio 类型的 MCP 服务，把它们的工具合并到一个端点，并校验 Token。配套的模板 Dockerfile 会预装一批服务。

### 1. 构建模板

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

- `MCP_SERVERS` 列出需要预装的服务。要用到的服务请全部预装：网关最多等待 55 秒，未预装的服务会在首次启动时下载。
- `BASE_IMAGE` 用于替换基础镜像。默认是 CubeSandbox 的 `sandbox-code` 镜像，其中已包含 `uv`、`node` 和 `git`。
- 如果使用 `mcp-gateway` 以外的别名，创建沙箱时显式传入 `template`，或为 Cube SDK 设置 `CUBE_MCP_TEMPLATE_ID`。详见[模板优先级](#模板优先级)。

等待模板状态变为 `READY`。

### 2. 创建沙箱并调用工具

```python
import asyncio
from cubesandbox import Sandbox
from mcp import ClientSession                                    # pip install mcp
from mcp.client.streamable_http import streamablehttp_client

async def main():
    with Sandbox.create(mcp={"time": {}, "fetch": {}}) as sandbox:
        headers = {"Authorization": f"Bearer {sandbox.get_mcp_token()}"}
        async with streamablehttp_client(sandbox.get_mcp_url(), headers=headers) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                print([tool.name for tool in (await session.list_tools()).tools])
                result = await session.call_tool("get_current_time", {"timezone": "Asia/Shanghai"})
                print(result.content[0].text)

asyncio.run(main())
```

如果本机解析不了 `*.<domain>`，可以把请求直接发给 CubeProxy，并把 `Host` 头设置为沙箱域名，做法与 SDK 使用 `CUBE_PROXY_NODE_IP` 时相同。

其他 SDK 提供相同的方法：

::: code-group

```ts [Node.js]
const sandbox = await Sandbox.create({ mcp: { time: {}, fetch: {} } });
const url = sandbox.getMcpUrl();
const token = await sandbox.getMcpToken();
```

```go [Go]
sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{
    MCP: cubesandbox.MCPServers{"time": map[string]any{}},
})
url := sandbox.GetMCPURL()
token, err := sandbox.GetMCPToken(ctx)
```

:::

### 示例网关支持的服务

`mcp` 是一个以服务名为键的对象，示例网关支持以下三类条目。

**目录服务**：值为该服务的属性。可在模板内执行 `mcp-gateway catalog` 查看完整列表。

| 名称 | 属性 | 实际运行 |
| --- | --- | --- |
| `duckduckgo` | | `uvx duckduckgo-mcp-server` |
| `arxiv` | `storagePath`（可选） | `uvx arxiv-mcp-server` |
| `fetch` | | `uvx mcp-server-fetch` |
| `time` | | `uvx mcp-server-time` |
| `filesystem` | `paths`（必填，列表） | `@modelcontextprotocol/server-filesystem` |
| `memory` | | `@modelcontextprotocol/server-memory` |
| `sequentialthinking` | | `@modelcontextprotocol/server-sequential-thinking` |

可以在模板中通过 `/etc/mcp-gateway/catalog.json` 新增或覆盖条目，格式与 `examples/mcp-gateway/internal/config/catalog.json` 相同。

**GitHub 服务**：与 E2B 一样，键名为 `github/<owner>/<repo>`。网关会克隆该仓库，在仓库目录中执行 `installCmd`，然后启动 `runCmd`：

```python
Sandbox.create(mcp={
    "github/acme/weather-mcp": {
        "installCmd": "npm ci",
        "runCmd": "node dist/index.js",
        "envs": {"WEATHER_API_KEY": "..."},
    },
})
```

**本地命令**：其他任何带 `runCmd` 的键都会直接运行该命令，适用于模板里已经装好的服务。

如果多个服务提供了同名工具，按服务名排序后的第一个服务保留原名，其余服务的工具会被重命名为 `<server>_<tool>`。网关只聚合工具，不对外提供 MCP 的 prompts 和 resources。端口 `50005` 上的 `GET /health` 无需 Token，返回服务数和工具数。

## 输入校验

以下情况 CubeAPI 会返回 `400`：

- `mcp` 不是对象；
- 某个条目不是对象；
- GitHub 键不符合 `github/<owner>/<repo>` 格式；
- GitHub 服务缺少 `runCmd` 或 `runCmd` 为空；
- `envs` 中有非法变量名或非字符串值；
- 服务数量超过 64 个。

服务名是否可识别由网关判断。示例网关遇到未知服务名会启动失败，SDK 会把这个错误报出来。

## 生命周期

使用示例网关时：

- **暂停 / 恢复**：网关及其服务进程属于内存快照的一部分，恢复后继续使用原 Token 提供服务。
- **重新连接**：`get_mcp_token()` 会读取 Token 文件，因此新的 SDK 句柄可以直接复用该端点。
- **克隆与快照恢复**：正在运行的网关及其 Token 会一并复制。所有克隆接受同一个 Token，应视为同一信任域。
- **进程崩溃**：某个服务进程退出后，网关会在下一次调用时重新启动它。

## 安全说明

- Token 只保护 MCP 端点本身。CubeProxy 鉴权和网络策略照常生效，沙箱内的服务同样受沙箱出网规则约束。
- **同一沙箱内的所有 MCP 服务属于同一信任域，彼此之间没有隔离。** 示例网关以 `root` 身份运行所有服务，任何一个服务都能读取其他服务的 `envs`（例如通过 `/proc/<pid>/environ`）和 `/etc/mcp-gateway/.token`，并可以把它们外传。GitHub 服务的 `installCmd` 和 `runCmd` 本身就是以 `root` 执行的任意命令。不要把不可信的服务和敏感凭据放进同一个沙箱；需要隔离时，为不同的信任域分别创建沙箱。
- 与 E2B 的约定一致，SDK 通过命令行参数传递 `mcp` 配置（`mcp-gateway --config '<json>'`）。在网关启动期间（最长约 60 秒），完整配置（包括 `envs`）会出现在该命令的进程参数中，沙箱内的任何进程都可以读到。
- 示例网关会从自身及每个 MCP 服务的环境变量中移除 Token，并且只把 `envs` 设置到所属服务的环境变量中，不写入磁盘或日志。这可以避免意外泄漏，但不能防范上面提到的恶意服务。
- 示例网关的日志 `/var/log/mcp-gateway.log` 会记录各服务的 `stderr`；如果服务自行打印密钥，密钥也会出现在其中。

## 使用自己的网关

你可以用任何满足[网关约定](#网关约定)的程序替换示例网关。例如，在 [Docker MCP Gateway](https://github.com/docker/mcp-gateway) 这类网关前面放一个名为 `mcp-gateway` 的轻量启动脚本；E2B 托管的 `mcp-gateway` 模板就是基于 Docker MCP Gateway 构建的。需要注意，Docker MCP Gateway 会把每个服务作为容器运行，因此模板里还需要一个能在沙箱内正常工作的 Docker 守护进程。CubeSandbox 尚未验证过这种方式。
