# Claude Code + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 内运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
（Anthropic 官方 CLI 编码 Agent）。Agent 在一个隔离、可复现的沙箱内编辑文件、执行命令并访问 Anthropic LLM API。

本示例包含：

- 一个 `Dockerfile`：在 CubeSandbox 基础镜像上叠加 Node.js 与 Claude Code CLI（envd 已监听 `:49983`）。
- `run_claude_code.py`：在 `/workspace` 内的一次性无交互运行。
- `resume_claude_code.py`：跨两轮的 pause/resume，证明 `/workspace` 在快照后仍存在。
- `network_policy.py`：默认拒绝出网的策略，由 CubeEgress 在链路上注入 API Key，密钥不进入 VM。
- `env_utils.py`、`.env.example`、`requirements.txt`。

## 目录结构

```
claude-code-integration/
├── Dockerfile            # CubeSandbox 模板镜像（Node.js + Claude Code CLI）
├── .env.example          # 复制为 .env 并填写
├── .gitignore
├── requirements.txt      # 宿主端驱动依赖（e2b、cubesandbox、python-dotenv）
├── env_utils.py          # .env 加载、Anthropic key、Claude Code 命令构造
├── _common.py            # 共享的沙箱命令辅助（run/ensure/id）
├── run_claude_code.py    # 一次性 Claude Code 任务
├── resume_claude_code.py # pause / resume 会话持久化
├── network_policy.py     # 默认拒绝出网 + 链路上注入密钥
├── README.md             # 英文文档
└── README_zh.md          # 中文文档（本文件）
```

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- 一个 Anthropic API Key（或通过 `ANTHROPIC_BASE_URL` 使用 Anthropic 兼容端点）。
- Python 3.10+（宿主端驱动脚本）。

## 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest
```

镜像会安装 `@anthropic-ai/claude-code`，以及 `git`、`python3`、`ripgrep`、`jq`，并清理
apt/npm 缓存。Claude Code 版本通过 `--build-arg CLAUDE_CODE_VERSION=x.y.z` 固定。

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

任务变为 `READY` 后记下 `template_id`。

## 3. 配置宿主端驱动

```bash
cd examples/claude-code-integration
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 以及你的 Anthropic key
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `ANTHROPIC_API_KEY` | `envs=...`（直连）或 CubeEgress 注入（vault） | Anthropic 密钥 |
| `ANTHROPIC_BASE_URL` | 传入 exec 环境 | Anthropic 兼容网关（如 DeepSeek） |
| `CLAUDE_CODE_MODEL` | Claude CLI `--model` 参数 | 默认 `claude-sonnet-4-6` |
| `CLAUDE_CODE_LLM_HOST` | `network_policy.py` | 放行的 LLM API host |

## 4. 一次性运行（直连注入密钥）

```bash
python run_claude_code.py --prompt "创建 hello.py 打印 'Hello from CubeSandbox' 并运行它。"
```

密钥通过 `sandbox.commands.run(..., envs=...)` 逐命令传入，只在该命令执行期间存在，不会写入 VM 内的持久文件。

> **安全：** 直连方式出网是放开的，Agent 被攻破可能外泄注入的密钥。共享集群请用保险柜方式（第 6 步）：默认拒绝出网 + 链路上注入密钥。

三个脚本都以 `--output-format stream-json` 运行 Claude Code，默认输出精简转写（助手文本、工具调用、失败项）。加 `--raw`（或设置 `CLAUDE_CODE_STREAM_RAW=1`）可改为打印原始 JSONL 事件流。

## 5. pause / resume（会话持久化）

```bash
python resume_claude_code.py
```

第一轮让 Claude Code 写 `/workspace/plan.md`，随后 `sandbox.pause()` 对 VM 打快照。脚本用
`Sandbox.connect(sandbox_id)` 恢复，校验 `/workspace/plan.md` 仍在，再执行第二轮续写。沙箱生命周期用 `try/finally` 手动管理（不用 context manager），避免 pause 后被过早 `kill` 掉。

## 6. 受限出网 + 密钥保险柜（推荐用于共享集群）

```bash
python network_policy.py
```

- 出网默认拒绝，仅放行 Anthropic LLM host。
- CubeEgress 在链路上把 Anthropic 密钥作为 HTTP 头注入（`x-api-key` 和 `anthropic-version`），因此沙箱内 `printenv` 看不到真实密钥，只有占位值。
- Claude Code 基于 Node.js（忽略系统 CA 库），脚本会设置 `NODE_EXTRA_CA_CERTS` 让 Claude Code 信任 CubeEgress 的拦截 CA；否则 vault 路径会以 `Connection error` 失败。若镜像里 CA 路径不同，可用 `CLAUDE_CODE_NODE_EXTRA_CA_CERTS` 覆盖。
- 任何其他目的地都会返回 `403 Forbidden - CubeEgress`。

若 Agent 需要访问额外主机（包镜像源、MCP 服务器等），请增加对应的放行规则，或把这些依赖预装进模板。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `claude: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| Anthropic 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 Claude Code 报 `Connection error` / TLS 失败 | Claude Code 基于 Node，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `CLAUDE_CODE_NODE_EXTRA_CA_CERTS` 覆盖 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |

## 参考

- 集成指南：[`docs/zh/guide/integrations/claude-code.md`](../../docs/zh/guide/integrations/claude-code.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 网络 / 出网策略示例：[`examples/network-policy`](../network-policy)
- Claude Code：<https://docs.anthropic.com/en/docs/claude-code>
