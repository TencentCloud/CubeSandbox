# OpenClaw (QClaw) + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 中运行 [OpenClaw](https://github.com/TencentCloud/CubeSandbox)
—— 腾讯的 AI 智能体网关。智能体网关以持久守护进程的形式运行在沙箱中，为 AI
智能体工作负载提供隔离、可复现的运行时环境。

OpenClaw 是 QClaw（腾讯 AI 桌面应用）和 CubeOps AgentHub 背后的核心智能体
运行时，支持多种 LLM provider（默认 DeepSeek，也支持 Anthropic、OpenAI），管理
智能体会话、工具执行和工作区状态。

本示例包含：

- `Dockerfile` —— 在 CubeSandbox 基础镜像之上安装 Node.js 22 和 OpenClaw 网关
  （envd 已在 `:49983` 上监听）。
- `run_qclaw_agent.py` —— 一次性运行：启动网关、发送 prompt、收集结果。
- `resume_qclaw_agent.py` —— 跨两轮对话的暂停/恢复，证明 `/workspace` 和
  `/root/.openclaw/` 在快照后仍然存活。
- `env_utils.py`、`.env.example`、`requirements.txt`。

## 目录结构

```
qclaw-sandbox/
├── Dockerfile               # CubeSandbox 模板镜像（Node.js + OpenClaw 网关）
├── .env.example             # 复制为 .env 并填写
├── .gitignore
├── requirements.txt         # 宿主机驱动依赖（e2b、python-dotenv）
├── env_utils.py             # .env 加载、provider 密钥、环境变量构建
├── _qclaw_common.py         # 共享工具（网关生命周期、HTTP 交互）
├── run_qclaw_agent.py       # 一次性 OpenClaw 任务
├── resume_qclaw_agent.py    # 暂停/恢复会话持久化
├── README.md                # 英文文档
└── README_zh.md             # 中文文档（本文件）
```

## 前置条件

- 运行中的 CubeSandbox 部署，CubeAPI 可通过 `http://<node>:3000` 访问。
- `cubemastercli` 在 `$PATH` 中，已连接到集群。
- 构建工作站上有 Docker，且有一个 Cube 节点可拉取的镜像仓库。
- LLM provider API 密钥（默认为 DeepSeek，也支持 Anthropic 和 OpenAI）。
- Python 3.10+ 用于运行宿主机驱动脚本。

## 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/qclaw-cube:latest \
  examples/qclaw-sandbox
docker push <your-registry>/qclaw-cube:latest
```

镜像安装 `openclaw`，以及 `git`、`python3`、`ripgrep`、`jq`、`supervisor`，
并清理 apt/npm 缓存。OpenClaw 版本通过 `--build-arg QCLAW_VERSION=x.y.z` 固定。

如果注册表需要内部 npm 源，请传入 `--build-arg NPM_REGISTRY=<url>`。

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/qclaw-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务状态变为 `READY` 后，记下 `template_id`。

## 3. 配置宿主机驱动

```bash
cd examples/qclaw-sandbox
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 和 provider 密钥
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发中可填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自步骤 2 |
| `QCLAW_PROVIDER` | 宿主机脚本 | `anthropic`、`deepseek`（默认） |
| `QCLAW_MODEL` | OpenClaw 配置 | Provider 的模型 ID |
| `DEEPSEEK_API_KEY` | `envs=...`（每次命令注入） | Provider 密钥（DeepSeek） |
| `ANTHROPIC_API_KEY` | `envs=...`（每次命令注入） | Provider 密钥（Anthropic） |

## 4. 一次性运行

```bash
python run_qclaw_agent.py --prompt "创建一个 hello.py，打印 'Hello from CubeSandbox' 并运行它。"
```

驱动脚本执行以下步骤：
1. 从模板创建沙箱
2. 通过 supervisor 启动 OpenClaw 网关
3. 等待网关就绪（端口 18789 + 认证令牌）
4. 通过网关 REST API 发送 prompt
5. 收集并显示响应
6. 销毁沙箱

## 5. 暂停 / 恢复（会话持久化）

```bash
python resume_qclaw_agent.py
```

第一轮对话让 OpenClaw 写入 `/workspace/plan.md`，然后 `sandbox.pause()` 对 VM
进行快照。脚本重新连接，验证 `/workspace/plan.md` 和 `/root/.openclaw/` 两者均
存活，重启网关，然后运行第二轮对话。

## 架构说明

- **网关守护进程模式**：与一次性 CLI 智能体不同，OpenClaw 作为由 `supervisor`
  管理的持久进程运行。网关处理智能体会话、工具执行和 LLM 通信。
- **状态目录**：`/root/.openclaw/` 保存配置、智能体状态、会话和工作区。此目录
  必须在暂停/恢复中存活以确保会话持久化。
- **多 provider**：通过 `QCLAW_PROVIDER` 环境变量支持 DeepSeek（默认）、
  Anthropic 和 OpenAI。
- **AgentHub 集成**：本模板是 CubeOps AgentHub 的配套组件，AgentHub 通过宿主机
  侧状态目录、出口凭证注入和快照/恢复来大规模管理 OpenClaw 实例。

## 故障排除

| 现象 | 可能原因 | 解决方法 |
|---|---|---|
| `openclaw: command not found` | CLI 变更后未重新构建模板 | 重新构建镜像，重新注册模板 |
| 网关 30 秒后未就绪 | Supervisor 配置缺失或端口冲突 | 检查 `supervisorctl status openclaw` 或 `/var/log/openclaw.log` |
| Provider 返回认证错误 | 密钥未转发或 provider 错误 | 检查 `QCLAW_PROVIDER` 和对应的 `*_API_KEY` 环境变量 |
| 网关返回 `403 Forbidden` | 启动和读取之间令牌不匹配 | 验证 `/root/.openclaw/openclaw.json` 中有有效令牌 |
| 就绪探针超时 | 镜像不含 envd | 确保 `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |

## 参考资料

- CubeOps AgentHub OpenClaw 服务：`CubeOps/internal/service/openclaw.go`
- CubeSandbox 快照 / 克隆： [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Pi 智能体示例： [`examples/pi-agent-integration`](../pi-agent-integration)
