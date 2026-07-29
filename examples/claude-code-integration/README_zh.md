# Claude Code 集成 CubeSandbox

在 CubeSandbox MicroVM 中运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
—— Anthropic 的 AI 编程 CLI 工具。本示例涵盖镜像构建、密钥注入、网络出口控制和基于快照的会话持久化。

[English](README.md)

## 快速开始

```bash
# 1. 配置环境变量
cp .env.example .env
# 填入: E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY

# 2. 安装依赖
pip install -r requirements.txt

# 3. 构建并注册模板镜像
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest

cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

# 4. 运行一次性任务
python run_claude_code.py

# 5. （可选）演示暂停/恢复
python resume_claude_code.py

# 6. （生产环境推荐）默认拒绝出口 + 凭证保险库
python network_policy.py
```

## 文件说明

| 文件 | 用途 |
|---|---|
| `Dockerfile` | 在 `cubesandbox-base:2026.16` 上安装 Node.js 24 + Claude Code CLI |
| `.env.example` | 环境变量参考 |
| `requirements.txt` | Python 依赖 (`e2b`, `cubesandbox`, `python-dotenv`) |
| `run_claude_code.py` | 一次性 headless Claude Code 任务 |
| `resume_claude_code.py` | 暂停/恢复持久化演示 |
| `network_policy.py` | CubeEgress 凭证保险库（默认拒绝出口 + 在线密钥注入） |
| `_cc_common.py` | 共享沙箱命令辅助函数 + JSON/JSONL 输出渲染 |
| `env_utils.py` | 环境变量工具函数 + CLI 命令构建器 |
| `test_cli.py` | CLI 验证测试套件（运行以验证 Claude Code CLI 行为） |

## 工作原理

### 架构

```
┌──────────────────────────┐     ┌─────────────────────────────┐
│   宿主机 (Python 驱动)     │     │   CubeSandbox MicroVM       │
│                          │     │                              │
│  Sandbox.create()   ─────┼────→│  envd (:49983)              │
│  sandbox.commands.run()  │────→│  claude -p "..." --json     │
│  sandbox.pause()         │────→│  /workspace (持久化)        │
│  Sandbox.connect()       │────→│  /root/.claude (状态目录)   │
└──────────────────────────┘     └─────────────────────────────┘
```

### 一次性任务 (`run_claude_code.py`)

创建沙箱、初始化演示项目、运行 `claude -p "<prompt>" --output-format json`、
解析结果，然后销毁沙箱。API 密钥通过 `envs=` 按命令注入——开发环境简单
易用，但密钥会进入 VM 内部。

### 会话持久化 (`resume_claude_code.py`)

- **第 1 轮**：Claude Code 写入 `/workspace/plan.md`，然后沙箱暂停。
- **暂停**：`sandbox.pause()` 对 VM 内存 + rootfs 做快照，释放计算资源。
- **第 2 轮**：`Sandbox.connect(sandbox_id)` 恢复沙箱，所有文件和
  Claude Code 状态完整保留，Claude Code 继续工作。

沙箱生命周期使用 `try/finally` 手动管理（而非 `with Sandbox.create()`），
以避免上下文管理器在暂停时销毁沙箱。

### 凭证保险库 (`network_policy.py`)

推荐的生产环境模式：

1. **默认拒绝出口** (`allow_internet_access=False`) — 除 Anthropic API 主机外，
   阻止所有出站流量。
2. **在线注入** — CubeEgress 在代理层附加 `x-api-key` 和
   `anthropic-version` 请求头。真实密钥永不进入 VM；
   `printenv ANTHROPIC_API_KEY` 只显示占位符。
3. **Node.js CA 信任** — Claude Code 运行在 Node.js 上，Node.js 使用自己的
   CA 证书库而忽略系统证书库。`NODE_EXTRA_CA_CERTS` 将 Node 指向包含
   CubeEgress 根证书的证书包（基础镜像已安装）。

### 输出格式

Claude Code 支持三种 `--output-format` 模式（均需配合 `-p`/`--print`）：

| 格式 | 行为 | 适用场景 |
|---|---|---|
| `json` | 单个 JSON 对象，含 `type`、`result`、`usage`、`is_error` 字段 | 一次性任务、结果捕获 |
| `stream-json` | JSONL 事件流（`system`/`assistant`/`result`）——需 `--verbose` | 实时流式、多轮对话 |
| `text` | 纯文本（默认） | 调试、人工阅读 |

示例脚本默认使用 `json`；如需流式，在 `cc_command()` 中添加
`--output-format stream-json --verbose`。

## 环境变量

| 变量 | 流向 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址 (`http://<node>:3000`) |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空值即可 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 从 `cubemastercli tpl create-from-image` 获取 |
| `ANTHROPIC_API_KEY` | `envs=...`（直接模式）或 CubeEgress 注入（保险库模式） | Anthropic API 密钥 |
| `ANTHROPIC_BASE_URL` | 传入 exec 环境 | API 网关/兼容端点 |
| `CC_MODEL` | `--model` 参数 | 默认：`claude-sonnet-4-6` |
| `CC_EFFORT` | `--effort` 参数 | 努力级别：low, medium, high, xhigh, max |
| `CC_LLM_HOST` | `network_policy.py` | 默认拒绝出口下允许的 API 主机 |
| `CC_PERMISSION_MODE` | `--permission-mode` 参数 | plan, acceptEdits, auto, bypassPermissions |

## 环境要求

- CubeSandbox 部署就绪，CubeAPI 可通过 `http://<node>:3000` 访问
- `cubemastercli` 已安装并连接到集群
- Docker，且 Cube 节点可拉取的镜像仓库
- Python 3.10+ 用于宿主机驱动脚本
- Anthropic API 密钥

## 注意事项

- **Node.js CA 信任（保险库模式）。** Claude Code 的 Node.js 运行时使用自己的
  CA 证书库。如果不设置 `NODE_EXTRA_CA_CERTS` 指向包含 CubeEgress 根证书的
  证书包，所有通过保险库路径的 API 调用都会因 TLS 错误而失败。
- **直接模式密钥持久化。** 直接模式 (`envs=`) 下的密钥作用域仅限于 exec 调用，
  但沙箱快照可能捕获 VM 内环境变量。如需严格隔离，请使用保险库模式。
- **仅支持 Headless 模式。** Claude Code 的交互式 TUI 无法在 E2B 协议上使用。
  请使用 `-p` / `--print` 配合 `--output-format json` 获取机器可读输出。
- **权限模式。** 在沙箱环境中，使用 `--permission-mode auto` 或
  `--dangerously-skip-permissions` 实现完全自主执行。`plan` 模式每次工具调用
  需要人工批准，不适用于 headless 沙箱场景。
- **出口副作用。** 执行 `npm install` 或获取 MCP 工具的任务，需将相关主机加入
  白名单或预先安装到模板中。
- **API 速率限制。** Claude Code 与 Anthropic API 交互，受标准速率限制和 Token
  配额约束。使用 `--max-budget-usd` 限制每次会话的开销。
- **状态目录。** `/root/.claude` 存放 Claude Code 会话状态。镜像中应保持为空，
  避免跨租户泄露会话信息。

## 技术背景

AI 编程 Agent（如 Claude Code、Pi、Gemini CLI）通常需要读写文件、执行命令和
安装依赖包。直接在开发工作站上运行会混合 Agent 的爆炸半径和本地开发环境。
CubeSandbox 通过 KVM MicroVM 提供硬件级隔离，每个会话拥有独立的客户内核。

相关研究领域包括：微虚拟机安全隔离 (Firecracker/gVisor)、容器逃逸检测、
基于 eBPF 的内核级沙箱、大语言模型工具调用的安全约束等。

## 参考资料

- [Claude Code 官方文档](https://docs.anthropic.com/en/docs/claude-code)
- [E2B Claude Code 集成](https://e2b.dev/docs/agents/claude-code)
- [CubeSandbox 自定义镜像](../../docs/guide/tutorials/bring-your-own-image.md)
- [CubeSandbox 快照/克隆/回滚](../../docs/guide/snapshot-rollback-clone.md)
- [CubeSandbox 安全代理（凭证保险库）](../../docs/guide/security-proxy.md)
