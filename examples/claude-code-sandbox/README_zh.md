# Claude Code + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 中运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
—— Anthropic 的官方 CLI 编程智能体。智能体在隔离、可复现的沙箱中编辑文件、运行命令、访问 Anthropic API。

本示例包含：

- `Dockerfile` —— 在 CubeSandbox 基础镜像之上安装 Node.js 22 和 Claude Code CLI（envd 已在 `:49983` 上监听）。
- `run_claude_agent.py` —— 在 `/workspace` 中进行一次性 headless 运行。
- `resume_claude_agent.py` —— 跨两轮对话的暂停/恢复，证明 `/workspace` 在快照后仍然存活。
- `env_utils.py`、`.env.example`、`requirements.txt`。

## 目录结构

```
claude-code-sandbox/
├── Dockerfile               # CubeSandbox 模板镜像（Node.js + Claude Code CLI）
├── .env.example             # 复制为 .env 并填写
├── .gitignore
├── requirements.txt         # 宿主机驱动依赖（e2b、python-dotenv）
├── env_utils.py             # .env 加载、密钥管理、claude 命令构建
├── _claude_common.py        # 共享沙箱命令工具（run/ensure/id）
├── run_claude_agent.py      # 一次性 Claude Code 任务
├── resume_claude_agent.py   # 暂停/恢复会话持久化
├── README.md                # 英文文档
└── README_zh.md             # 中文文档（本文件）
```

## 前置条件

- 运行中的 CubeSandbox 部署，CubeAPI 可通过 `http://<node>:3000` 访问。
- `cubemastercli` 在 `$PATH` 中，已连接到集群。
- 构建工作站上有 Docker，且有一个 Cube 节点可拉取的镜像仓库。
- Anthropic API 密钥。
- Python 3.10+ 用于运行宿主机驱动脚本。

## 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-sandbox
docker push <your-registry>/claude-code-cube:latest
```

镜像安装 `@anthropic-ai/claude-code`，以及 `git`、`python3`、`ripgrep`、`jq`，
并清理 apt/npm 缓存。Claude Code 版本通过 `--build-arg CLAUDE_CODE_VERSION=x.y.z` 固定。

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务状态变为 `READY` 后，记下 `template_id`。

## 3. 配置宿主机驱动

```bash
cd examples/claude-code-sandbox
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 和 ANTHROPIC_API_KEY
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发中可填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自步骤 2 |
| `ANTHROPIC_API_KEY` | `envs=...`（每次命令注入） | Provider 密钥 |

## 4. 一次性运行

```bash
python run_claude_agent.py --prompt "创建一个 hello.py，打印 'Hello from CubeSandbox' 并运行它。"
```

密钥通过 `sandbox.commands.run(..., envs=...)` 按命令转发，只在 exec 调用期间存在——不会写入 VM 内的持久文件。

## 5. 暂停 / 恢复（会话持久化）

```bash
python resume_claude_agent.py
```

第一轮对话让 Claude Code 写入 `/workspace/plan.md`，然后 `sandbox.pause()` 对 VM
进行快照。脚本通过 `Sandbox.connect(sandbox_id)` 重新连接，验证 `/workspace/plan.md`
存活，然后运行第二轮对话继续工作。

## 故障排除

| 现象 | 可能原因 | 解决方法 |
|---|---|---|
| `claude: command not found` | CLI 变更后未重新构建模板 | 重新构建镜像，重新注册模板 |
| Anthropic 返回认证错误 | 密钥未转发 | 通过 `envs={...}` 传入 `ANTHROPIC_API_KEY` |
| 就绪探针超时 | 镜像不含 envd | 确保 `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` 报错 | 平台版本过旧不支持快照 | 升级 CubeSandbox 平台 |

## 参考资料

- Claude Code 文档：<https://docs.anthropic.com/en/docs/claude-code>
- CubeSandbox 快照 / 克隆： [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Pi 智能体示例： [`examples/pi-agent-integration`](../pi-agent-integration)
