# OpenCode × CubeSandbox 集成

在 CubeSandbox MicroVM 内运行 [OpenCode](https://www.npmjs.com/package/opencode-ai)（面向终端的 AI 编码 Agent），具备硬件级隔离、基于快照的会话持久化，以及可选的默认拒绝出网 + 链路上凭证注入。

> 完整集成指南：[`docs/zh/guide/integrations/opencode.md`](../../docs/zh/guide/integrations/opencode.md)

## 前置条件

| 要求 | 说明 |
|---|---|
| CubeSandbox 部署 | CubeAPI 可访问（`http://<node>:3000`） |
| `cubemastercli` | 已在 `$PATH` 且已连通集群 |
| Python | 3.10+ |
| Docker | 构建机 + Cube 集群可拉取的 registry |
| LLM provider API key | `anthropic`、`openai`、`deepseek` 或 `openrouter` |

## 快速开始

### 1. 构建并注册模板

```bash
# 以下环境变量均为可选；默认值如注释所示
# REGISTRY=ghcr.io/tencentcloud  IMAGE_NAME=opencode-cube  IMAGE_TAG=latest
./build-template.sh
```

脚本会构建 Docker 镜像、推送、通过 `cubemastercli tpl create-from-image` 注册，监控构建任务，并在到达 `READY` 后打印最终的 `template_id`。

### 2. 配置 `.env`

```bash
cp .env.example .env
# 填写 CUBE_API_URL、CUBE_TEMPLATE_ID 以及你的 provider key
pip install -r requirements.txt
```

### 3. 运行一次性示例

```bash
python run_opencode.py
```

脚本会创建 MicroVM，植入确定性的计算器项目，执行 OpenCode 编码任务，验证结果中包含预期标记，最后销毁沙箱。

## 文件说明

| 文件 | 描述 |
|---|---|
| `build-template.sh` | 构建、推送并注册模板镜像 |
| `Dockerfile` | 在 `cubesandbox-base` 上叠加 Node.js 24 + OpenCode CLI |
| `run_opencode.py` | 一次性示例：植入 → 运行 → 验证 |
| `snapshot_restore.py` | 暂停/恢复：第一轮 → pause → 重连 → 第二轮 |
| `network_policy.py` | 默认拒绝出网 + CubeEgress 凭证保险柜 |
| `env_utils.py` | 配置、provider 规格、密钥脱敏工具 |
| `_opencode_common.py` | 共享命令构造、结果解析、session ID 提取 |
| `test_commands.py` | `_opencode_common.py` 单元测试 |
| `test_env_utils.py` | `env_utils.py` 单元测试 |
| `.env.example` | 环境变量模板 |
| `requirements.txt` | Python 依赖（`cubesandbox`、`python-dotenv`） |

## 环境变量

完整模板见 `.env.example`。主要变量：

| 变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `CUBE_API_URL` | 是 | — | CubeAPI 地址（`http://<node>:3000`） |
| `CUBE_TEMPLATE_ID` | 是 | — | 来自 `build-template.sh` 的模板 ID |
| `OPENCODE_PROVIDER` | 是 | `anthropic` | LLM provider：`anthropic`、`openai`、`deepseek`、`openrouter` |
| `OPENCODE_MODEL` | 是 | — | 必须使用 `provider/model` 格式（如 `anthropic/claude-sonnet-4-6`） |
| `ANTHROPIC_API_KEY` | 按 provider | — | provider 密钥（只需填写所选 provider 的 key） |
| `OPENAI_API_KEY` | 按 provider | — | — |
| `DEEPSEEK_API_KEY` | 按 provider | — | — |
| `OPENROUTER_API_KEY` | 按 provider | — | — |
| `OPENCODE_LLM_HOST` | 否 | provider 默认值 | 覆盖 LLM API 主机名，用于 CubeEgress 规则 |
| `OPENCODE_WORKSPACE` | 否 | `/workspace` | 沙箱内工作区路径 |
| `OPENCODE_SANDBOX_TIMEOUT` | 否 | `1800` | 沙箱生命周期（秒） |
| `OPENCODE_EXEC_TIMEOUT` | 否 | `900` | 单命令超时（秒） |
| `OPENCODE_NODE_CA_BUNDLE` | 否 | `/etc/ssl/certs/ca-certificates.crt` | CubeEgress TLS CA 包（保险柜方式） |
| `OPENCODE_PLACEHOLDER_KEY` | 否 | `cube-egress-managed-placeholder` | VM 内的占位 key 值（保险柜方式） |
| `CUBE_PROXY_NODE_IP` | 否 | — | SDK 数据路径的直接节点路由 |

## 快照与恢复

```bash
python snapshot_restore.py
```

运行两轮工作流：

1. **第一轮** — OpenCode 创建 `plan.md` 描述计划内容。
2. **暂停** — `sandbox.pause()` 对运行中的 VM（内存 + rootfs）打快照。
3. **重连** — `Sandbox.connect(sandbox_id)` 恢复，`/workspace` 与 OpenCode 状态目录完好无损。
4. **第二轮** — OpenCode 带 `--continue` 继续，执行计划并跑测试。

> 用 `try/finally`（而非 `with` context manager）管理沙箱生命周期。context manager 在 `__exit__` 时会 kill 沙箱，导致 pause 失效。

## 默认拒绝出网与凭证保险柜

```bash
python network_policy.py
```

创建 `allow_internet_access=False` 的沙箱，仅放行一条针对已配置 LLM host 的规则。真实 provider key 留在宿主；VM 内只有占位值。当 OpenCode 调用 LLM API 时，CubeEgress 在链路上注入真实鉴权头。

- 沙箱内 `printenv ANTHROPIC_API_KEY` 只显示占位值。
- 每次访问放行的 LLM host 都会在链路上被附加鉴权头。
- 其他所有目的地都被 CubeVS 在 L3/L4 层丢弃。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| `opencode: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（保险柜） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host 加入网络规则 |
| `Connection error` / TLS 失败（保险柜） | Node.js 忽略系统 CA 库 | 示例已设 `NODE_EXTRA_CA_CERTS`；用 `OPENCODE_NODE_CA_BUNDLE` 覆盖 |
| 模板卡在 `PULLING` | Cube 节点无法访问 registry | 推送到可访问的 registry，必要时提供鉴权 |
| 就绪探针超时 | 基础镜像缺少 envd | 使用 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `Missing required environment variable` | `.env` 未配置 | 复制 `.env.example` 并填写必填项 |
| `OPENCODE_MODEL` 校验失败 | 模型缺少 `provider/model` 格式 | 使用如 `anthropic/claude-sonnet-4-6` |

## 完整指南

如需了解架构、最佳实践和注意事项的更深入说明，请参阅
[OpenCode 集成指南](../../docs/zh/guide/integrations/opencode.md)。
