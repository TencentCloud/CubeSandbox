# MiMo Code + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 中运行
[MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)。MiMo Code 是基于
OpenCode 演进的终端编码 Agent，增加了持久记忆、Checkpoint、子 Agent 和
Compose 工作流等能力。

本示例包含：

- 固定版本的 MiMo Code 模板镜像；
- 使用 NDJSON 输出的无头一次性任务；
- `pause()` / `Sandbox.connect()` 后精确续接同一个会话；
- 默认拒绝的 CubeEgress 与 MiMo Platform 密钥出口注入；
- 无需额外编排层的可选 MiMo Compose 模式。

## 目录结构

```text
mimo-code-integration/
├── Dockerfile
├── build-template.sh
├── .env.example
├── .gitignore
├── requirements.txt
├── env_utils.py
├── _mimo_common.py
├── run_mimo_code.py
├── resume_mimo_code.py
├── network_policy.py
├── tests/
├── README.md
└── README_zh.md
```

## 前置条件

- CubeSandbox 已运行，且可通过 `http://<cube-host>:3000` 访问 CubeAPI；
- 构建机上有 `cubemastercli`、Docker，以及 Cube 节点可拉取的镜像仓库；
- Host 侧安装 Python 3.10+；
- 从 <https://platform.xiaomimimo.com> 获取的 MiMo Platform API Key；
- CubeSandbox 平台 `>= 0.3.0`（pause/resume）和 `>= 0.4.0`
  （CubeEgress 凭证注入）。

首版示例只面向 MiMo Platform，不根据任意 URL 猜测 Provider 或鉴权方式。

## 1. 构建并注册模板

便捷脚本会构建 `linux/amd64` 镜像、推送镜像并提交模板导入：

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

对应的手动命令：

```bash
docker build --platform linux/amd64 \
  --build-arg MIMO_VERSION=0.1.7 \
  -t <your-registry>/mimo-code-cube:0.1.7 \
  examples/mimo-code-integration
docker push <your-registry>/mimo-code-cube:0.1.7

cubemastercli tpl create-from-image \
  --image <your-registry>/mimo-code-cube:0.1.7 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

镜像固定安装 `@mimo-ai/cli@0.1.7`，构建时执行 `mimo --version`，并继承
CubeSandbox 的 `envd` entrypoint。

## 2. 配置 Host 驱动

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
# 设置 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID 和 MIMO_API_KEY。
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

| 变量 | 用途 |
| --- | --- |
| `E2B_API_URL` / `E2B_API_KEY` | CubeAPI 连接信息 |
| `CUBE_TEMPLATE_ID` | 状态为 READY 的模板 ID |
| `MIMO_API_KEY` | MiMo Platform 凭证 |
| `MIMO_MODEL` | 默认 `mimo/mimo-v2.5-pro` |
| `MIMOCODE_HOME` | 绝对 profile 根目录，默认 `/root/.mimocode` |
| `MIMO_WORKSPACE` | Agent 工作目录，默认 `/workspace` |
| `MIMO_SANDBOX_TIMEOUT` | 沙箱空闲超时，默认 `1800` 秒 |
| `MIMO_AGENT_EXEC_TIMEOUT` | MiMo 命令超时，默认 `900` 秒 |
| `MIMO_NODE_EXTRA_CA_CERTS` | 仅供 `network_policy.py` 使用的 CA 证书包，默认系统证书包 |

`MIMOCODE_HOME` 会把 `config/`、`data/`、`state/` 和 `cache/` 统一到一个
profile 根目录。MiMo 的会话数据库、持久记忆和 Checkpoint 因而会随
CubeSandbox 快照一起保存。

远程且开启鉴权的 CubeAPI 应使用 HTTPS。明文 HTTP 只适合受信任的本地部署，不能
让真实 Cube API Key 穿过不可信网络。

### 端到端测试前置检查与证据

执行真实测试前，确认 `cubemastercli tpl list` 中 `.env` 配置的模板状态为
`READY`、CubeAPI 健康，并且 Host Python 环境能导入两个 SDK：

```bash
cubemastercli tpl list
curl -fsS http://<cube-host>:3000/health
python -c 'import e2b, cubesandbox; print("SDK dependencies OK")'
```

执行 `network_policy.py` 前，还应确认 CubeEgress 正在运行、审计日志可写入
`/data/log/cube-egress/access.jsonl`，以及设置了
`MIMO_NODE_EXTRA_CA_CERTS` 时该证书包包含 CubeEgress CA。模板 ID 仅属于一个
CubeSandbox 集群：每个集群都应自行导入镜像，并将新建且状态为 `READY` 的模板 ID
写入本地 `.env`；不要将环境专属 ID 写入 `.env.example`。

将脱敏后的命令输出保存在 `output/`（已被 Git 忽略），记录镜像 digest、模板状态、
沙箱/会话 ID、结果 marker 以及最终的 `sandboxes: 0` 健康响应。为 issue 或 PR
准备材料时，对同一份脱敏终端或控制台证据截图。不要保存 `.env`、真实 API Key、
仓库凭证或完整鉴权请求头。

## 3. 执行一次性任务

```bash
python run_mimo_code.py
```

自定义 Prompt 必须创建带有 `CUBE_MIMO_RUN_OK` 的 `result.md`；如果任务使用其他
输出契约，请传入 `--skip-result-check`。

脚本会创建一个极小的 Python 项目，然后执行：

```bash
mimo run --format json --dir /workspace \
  --model mimo/mimo-v2.5-pro \
  --agent build \
  --dangerously-skip-permissions "<prompt>"
```

脚本解析 MiMo 的 NDJSON 事件，展示工具和文本事件，提取 `sessionID`，并验证
Agent 生成的 `result.md`。

直接模式通过 `commands.run(..., envs=...)` 只向本次 MiMo 进程传递
`MIMO_API_KEY`。这适合开发验证，但拥有任意出口权限的 Agent 仍可能泄露密钥。
共享环境应使用 CubeEgress 模式。

> `--dangerously-skip-permissions` 会自动批准没有被显式拒绝的工具。只应在
> 没有 Host 挂载和无关秘密的隔离、一次性沙箱中使用。

## 4. 暂停并续接同一个 MiMo 会话

```bash
python resume_mimo_code.py
```

脚本会：

1. 启动第一轮 MiMo 任务并从 NDJSON 中提取 `sessionID`；
2. 要求 MiMo 记住随机 token，但不把它写入 `/workspace`；
3. 暂停 MicroVM，并通过 `Sandbox.connect()` 重新连接；
4. 验证 `/workspace`、`$MIMOCODE_HOME/data` 和 `mimo session list`；
5. 使用 `mimo run --session <id>` 执行第二轮；
6. 验证 MiMo 能回忆 token 并继续原任务。

这验证的是 Agent 对话连续性，而不只是文件仍然存在。脚本刻意不使用
`with Sandbox.create(...)`，因为退出上下文会直接 kill 沙箱，无法恢复。

MiMo Checkpoint 与 CubeSandbox 快照解决不同问题：前者用于压缩和重建模型
上下文；后者保存完整 VM 内存、根文件系统、workspace、数据库和 profile。

## 5. 限制出口并让真实密钥留在 VM 外

```bash
python network_policy.py
```

这是共享集群的推荐模式：

- `allow_internet_access=False` 拒绝所有未匹配流量；
- 只允许 `api.xiaomimimo.com`；
- VM 内只能看到 `MIMO_API_KEY=cube-egress-managed-placeholder`；
- CubeEgress 把请求中的 `api-key` 替换为真实密钥；
- `NODE_EXTRA_CA_CERTS` 让 MiMo 运行时信任 CubeEgress CA；
- `example.com` 必须被阻断：请求到达 CubeEgress 时返回 `403`，在 L3 层执行默认
  拒绝的部署中 curl 会返回 `000`；无论哪种情况，已鉴权的 MiMo 任务都必须成功。

内联 `MIMOCODE_CONFIG_CONTENT` 只包含 `{env:MIMO_API_KEY}`，不包含真实密钥。
示例同时关闭分享、遥测、更新检查、远程模型清单、LSP 下载和外部 Skills，
因此出口白名单可以保持最小。

## 6. 使用 MiMo Compose

Compose 是 MiMo 的主多 Agent 模式，可直接通过同一个 runner 使用：

```bash
python run_mimo_code.py --agent compose \
  --prompt "Inspect app.py, improve it, run it, and write result.md containing CUBE_MIMO_RUN_OK."
```

Compose 是否以及如何委派任务由模型决定，因此基础 smoke test 不断言具体子 Agent
行为，只验证最终产物和固定 marker。

## 测试

```bash
python -m unittest discover -s tests -v
python -m py_compile *.py
bash -n build-template.sh
```

离线测试覆盖配置生成、密钥排除、命令转义、MiMo Platform 请求头、分块 NDJSON
和 sessionID 解析。完整端到端流程仍需要真实集群和 API 凭证。

## 常见问题

| 现象 | 可能原因 | 处理方式 |
| --- | --- | --- |
| `mimo: command not found` | 模板未更新 | 重新构建镜像并导入新模板 |
| 平台二进制不受支持 | 镜像架构和 Cube 节点不一致 | 使用 `--platform linux/amd64` 或匹配节点的受支持架构 |
| MiMo 鉴权失败 | Key 无效或请求头错误 | 设置 `MIMO_API_KEY`；MiMo Platform 使用 `api-key`，不是 Bearer |
| `403 Forbidden - CubeEgress` | Host 没有命中精确规则 | 保持端点为 `api.xiaomimimo.com`，并查看审计日志 |
| TLS/证书错误 | MiMo 运行时不信任 CubeEgress CA | 将 `MIMO_NODE_EXTRA_CA_CERTS` 指向包含该 CA 的证书包 |
| models.dev、遥测或更新请求失败 | 窄白名单下的预期行为 | 保留示例中的 disable 开关 |
| 模板停在 `PULLING` | 节点无法访问镜像仓库 | 使用节点可访问的仓库并配置拉取凭证 |
| readiness probe 超时 | 镜像未继承 Cube entrypoint | 使用固定的 CubeSandbox base image |
| 输出中没有 `sessionID` | CLI 版本或输出模式变化 | 使用固定版本和 `--format json` |
| 恢复后会话消失 | profile/workspace 不一致或快照失败 | 保持相同绝对 `MIMOCODE_HOME`，检查 pause/connect 错误 |
| 命令超时 | 模型或工具任务超出默认时间 | 增大 `MIMO_AGENT_EXEC_TIMEOUT` 和沙箱生命周期 |

## 安全说明

- 会话数据库和持久记忆可能包含提示词、源码、路径和命令输出。应限制快照访问，并
  及时 kill 不再使用的沙箱。
- OAuth 会把 access/refresh token 存入 `auth.json`；本示例不采用 OAuth，因为
  快照会持久化这些 token。
- 发布 `mimo export` 内容前必须检查并脱敏。
- 除非任务确实需要，不要把包仓库、MCP Server 或任意域名加入 CubeEgress 白名单。

## 参考

- [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code CLI 参数](https://mimo.xiaomi.com/mimocode/cli-options)
- [MiMo Code 会话](https://mimo.xiaomi.com/mimocode/sessions)
- [CubeSandbox 集成指南](../../docs/zh/guide/integrations/mimo-code.md)
- [CubeSandbox 生命周期](../../docs/zh/guide/lifecycle.md)
- [CubeEgress 安全代理](../../docs/zh/guide/security-proxy.md)
