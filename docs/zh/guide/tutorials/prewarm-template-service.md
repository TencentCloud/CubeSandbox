# 预热模板服务

普通模板可以预装系统和依赖，但应用仍可能在每次沙箱启动后执行耗时初始化，例如加载 SDK、创建 Agent 会话、扫描工作区或建立本地缓存。CubeSandbox 可以等待应用完成这些工作后再制作模板快照，使后续沙箱从已初始化的内存和进程状态恢复。

本文以 [`examples/pi-agent-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration) 为例，介绍如何设计和制作预热模板。开始前建议先阅读[模板概览](../templates.md)，了解探针如何决定制作快照的时机。

## 工作原理

预热模板的关键是让应用提供一个准确的就绪端点：

1. 镜像启动常驻服务。
2. 服务完成需要预热的初始化工作。
3. 初始化完成后，就绪端点才返回 HTTP 2xx。
4. CubeSandbox 探测到成功响应，保存此时的文件系统、内存和进程状态。
5. 从模板创建沙箱时，常驻服务随快照恢复，可以直接处理请求。

就绪端点不应在 HTTP 服务器刚开始监听时就返回成功。应先返回 503，直到所有需要保存在快照中的状态都已准备完成。

## Pi Agent 预热示例

Pi Agent 的普通运行方式会为每个任务启动新进程。示例中的 warmup adapter 则作为镜像的常驻进程，在启动阶段创建一个 Pi SDK `AgentSession`。只有会话初始化完成后，`GET /readyz` 才返回 200。

### 1. 准备常驻服务

示例实现位于 [`pi_warmup_adapter.mjs`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/pi_warmup_adapter.mjs)。其核心逻辑可以概括为：

```javascript
let ready = false;

const server = http.createServer((request, response) => {
  if (request.method === "GET" && request.url === "/readyz") {
    response.writeHead(ready ? 200 : 503);
    return response.end();
  }

  // 处理恢复后的业务请求。
});

session = await createAgentSession(/* ... */);
ready = true;
server.listen(8080, "0.0.0.0");
```

实际示例还提供 `POST /prompt`，用于向恢复后的常驻 `AgentSession` 发送任务。

### 2. 将常驻服务设为镜像命令

[`Dockerfile.warmup`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/Dockerfile.warmup) 基于已安装 Pi Agent 的镜像，复制 adapter，并将其设为镜像的 `CMD`：

```dockerfile
ARG PI_AGENT_IMAGE=pi-agent-cube:latest
FROM ${PI_AGENT_IMAGE}

COPY pi_warmup_adapter.mjs /tmp/pi_warmup_adapter.mjs
RUN PI_PACKAGE_DIR="$(npm root -g)/@earendil-works/pi-coding-agent" \
    && install -m 0755 /tmp/pi_warmup_adapter.mjs \
       "${PI_PACKAGE_DIR}/pi_warmup_adapter.mjs" \
    && rm /tmp/pi_warmup_adapter.mjs

ENV PI_WARMUP_HOST=0.0.0.0 \
    PI_WARMUP_PORT=8080

EXPOSE 49983 8080

CMD ["sh", "-c", "exec node \"$(npm root -g)/@earendil-works/pi-coding-agent/pi_warmup_adapter.mjs\""]
```

端口 `8080` 提供应用就绪检查和任务接口；`49983` 由基础镜像中的 `envd` 使用，以保留 SDK 的命令、文件和终端能力。

### 3. 构建并推送镜像

在仓库根目录先构建基础镜像，再构建 warmup 镜像：

```bash
docker build --platform linux/amd64 \
  -t localhost:5000/pi-agent-cube:latest \
  examples/pi-agent-integration

docker build --platform linux/amd64 \
  -f examples/pi-agent-integration/Dockerfile.warmup \
  --build-arg PI_AGENT_IMAGE=localhost:5000/pi-agent-cube:latest \
  -t localhost:5000/pi-agent-warmup-cube:latest \
  examples/pi-agent-integration

docker push localhost:5000/pi-agent-cube:latest
docker push localhost:5000/pi-agent-warmup-cube:latest
```

请将示例地址替换为 CubeSandbox 集群能够访问的镜像仓库。

### 4. 使用应用探针制作模板

创建模板时暴露 `envd` 和 warmup adapter 的端口，但将探针指向真正代表 Pi 会话初始化完成的 `/readyz`：

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-warmup-cube:latest \
  --alias pi-warmup \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 8080 \
  --probe-path /readyz
```

这里不能使用 `49983/health` 作为预热完成信号：它只能说明 `envd` 已经就绪，不能说明 Pi `AgentSession` 已创建完成。而是应当等待 `/readyz` 就绪，此时所有的服务才均已启动完成。

## 设计自己的预热服务

将同一模式应用到其他服务时，请遵循以下原则：

- **探测真实的预热状态。** 完成模型加载、运行时初始化或缓存构建后，再让就绪端点返回 2xx。
- **保持进程常驻。** 完成初始化的进程必须继续运行，才能随内存快照一起恢复。
- **不要把密钥写入模板。** 构建期间不要注入 API Key、令牌或用户数据；应在沙箱恢复后通过请求、密钥保险柜或其他运行时机制提供。
- **谨慎处理外部连接。** 数据库连接、长连接和临时凭证在恢复时可能已失效。恢复后应检测并重建这类连接，而不是假设快照中的连接仍可使用。
- **明确并发模型。** Pi 示例中的一个 adapter 只维护一个 session，并发任务返回 HTTP 409。需要并发时，应实现连接池、会话池，或让每个沙箱只处理一个任务。
- **保持就绪检查轻量。** 探针应只读取本地状态，不应重复执行昂贵初始化或产生外部副作用。

## 排查问题

| 现象 | 检查项 |
| --- | --- |
| 模板构建一直等待探针 | 确认服务监听 `0.0.0.0`、端口和路径匹配，并查看初始化日志。 |
| 模板 READY，但应用首次请求仍需初始化 | 就绪端点返回过早；将 `ready` 状态设置移到完整初始化之后。 |
| SDK 命令或文件 API 不可用 | 确认镜像包含 `envd`，并通过 `--expose-port 49983` 暴露其端口。 |
| 恢复后外部请求失败 | 检查快照前建立的连接或凭证是否过期，并在恢复后重新建立。 |

完整 Pi Agent 的构建、调用和网络策略示例见 [`examples/pi-agent-integration/README_zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/pi-agent-integration/README_zh.md)。
