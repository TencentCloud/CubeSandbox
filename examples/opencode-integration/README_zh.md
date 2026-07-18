# OpenCode + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 内运行 [OpenCode CLI](https://opencode.ai/)
（开源、面向终端的 AI 编码 Agent）。Agent 在一个隔离、可复现的沙箱内编辑
文件、执行命令并访问 LLM API。

本示例包含：

- 一个 `Dockerfile`：在 CubeSandbox 基础镜像上叠加 Node.js 20 与 OpenCode CLI
  （envd 已监听 `:49983`）。
- `run_opencode.py`：在 `/workspace` 内的一次性无交互运行。
- `resume_opencode.py`：跨两轮的 pause/resume，证明 `/workspace`、OpenCode
  配置目录（`/workspace/.opencode/config/`）与数据目录
  （`/workspace/.opencode/data/`，会话存放在这）都在快照后保留。
- `network_policy.py`：默认拒绝出网的策略，由 CubeEgress 在链路上注入 API
  Key，密钥不进入 VM。
- `sandbox_exec.py`：宿主端 CLI 执行器，让你在一次性 MicroVM 内跑任意 Python
  或 shell 代码（`--code`、`--file`、`--cmd`、`--pip`），通过
  `/tmp` 下按 UID 隔离的会话文件跨调用复用同一沙箱。
- `mcp_server.py`：把同一执行后端暴露成 MCP server（JSON-RPC over stdio），
  任何 MCP 客户端（Claude Desktop、Cursor 等）都能通过 OpenCode 工具链将
  代码沙箱化执行。
- `hooks/`：OpenCode JavaScript 插件（`cubesandbox-sandbox.js`），把 Agent 内
  的 `bash` 工具调用转发到宿主端执行器；附带 `install.sh`，把插件拷到
  `~/.config/opencode/plugins/`。与 Claude Code 集成的 PreToolUse-Bash
  模式对齐。
- `env_utils.py`、`_opencode_common.py`、`.env.example`、`requirements.txt`、
  `tests/`。

## 目录结构

```
opencode-integration/
├── Dockerfile            # CubeSandbox 模板镜像（Node.js + OpenCode CLI）
├── .env.example          # 复制为 .env 并填写
├── .gitignore
├── requirements.txt      # 宿主端驱动依赖（e2b、cubesandbox、python-dotenv）
├── env_utils.py          # .env 加载、provider key、OpenCode 命令构造
├── _opencode_common.py   # 共享的沙箱命令辅助（run/ensure/id）
├── run_opencode.py       # 一次性 OpenCode 任务
├── resume_opencode.py    # pause / resume 会话持久化
├── network_policy.py     # 默认拒绝出网 + 链路上注入密钥
├── sandbox_exec.py       # 宿主端 CLI：--code / --file / --cmd / --pip
├── mcp_server.py         # JSON-RPC stdio MCP server，提供 5 个沙箱工具
├── hooks/
│   ├── install.sh                       # 把插件和脱敏后的配置写入 ~/.config/opencode
│   └── cubesandbox-sandbox.js           # tool.execute.before 插件，把 bash 调用转交沙箱
├── tests/                # pytest 套件（与 Claude Code 测试结构对齐）
│   ├── conftest.py
│   ├── test_env_utils.py
│   ├── test_opencode_common.py
│   ├── test_sandbox_exec.py
│   └── test_mcp_server.py
├── README.md             # 英文文档
└── README_zh.md          # 中文文档（本文件）
```

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- OpenCode 兼容的 LLM provider 密钥 —— Anthropic、OpenAI、Google Gemini、
  DeepSeek、OpenRouter、Groq、Mistral，或任何 OpenCode 内置预设的 provider。
  见 `.env.example`。
- Python 3.10+（宿主端驱动脚本）。

## 1. 构建模板镜像

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/opencode-cube:latest \
  examples/opencode-integration
docker push <your-registry>/opencode-cube:latest
```

镜像会安装 `opencode-ai`，以及 `git`、`python3`、`ripgrep`、`jq`，并清理
apt/npm 缓存。OpenCode 版本通过 `--build-arg OPENCODE_VERSION=x.y.z` 固定。
**已针对 `opencode-ai@1.17.20` 测试**——脚本依赖 OpenCode 1.17 才加入的
`--dangerously-skip-permissions` 标志；更老的版本需要用等价的
`OPENCODE_PERMISSION='{"*":"allow"}'` 环境变量。

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务变为 `READY` 后记下 `template_id`。

## 3. 配置宿主端驱动

```bash
cd examples/opencode-integration
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 以及 provider 密钥
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `OPENCODE_PROVIDER` | `env_utils.provider()` | 可选 —— 未设置时从已配的密钥反推 |
| `OPENCODE_MODEL` | OpenCode CLI | 对应 provider 的模型 id |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GOOGLE_API_KEY` / ... | `envs=...`（直连）或 CubeEgress 注入（vault） | provider 密钥 |
| `OPENCODE_BASE_URL` / `ANTHROPIC_BASE_URL` | OpenCode CLI | 自定义上游端点（如通过 Anthropic 兼容网关接 DeepSeek） |
| `OPENCODE_LLM_HOST` | `network_policy.py` | 放行的 LLM API host，默认从 `*_BASE_URL` 解析或取 provider 默认 |

## 4. 一次性运行（直连注入密钥）

```bash
python run_opencode.py --prompt "创建 hello.py 打印 'Hello from CubeSandbox' 并运行它。"
```

OpenCode 以无交互模式启动：`opencode run "..."` 让它处理完 prompt 即退出（不进
入交互 TUI）。脚本会在命令行追加 `--dangerously-skip-permissions`
（OpenCode 1.17+，别名 `--yolo`）让所有工具调用在本次运行期间自动放行——
否则 CLI 会卡在无法在 exec 信道回答的权限弹窗上。密钥通过
`sandbox.commands.run(..., envs=...)` 逐命令传入，只在该命令执行期间存在，不会
写入 VM 内的持久文件。传 `--no-approve` 可以去掉该 flag，让 OpenCode 走权限
弹窗（前提是用 `opencode.json` 把工具白名单收紧过，否则会卡住）。

> **安全：** 直连方式出网是放开的，Agent 被攻破可能外泄注入的密钥。共享集群请
> 用保险柜方式（第 6 步）：默认拒绝出网 + 链路上注入密钥。

## 5. pause / resume（会话持久化）

```bash
python resume_opencode.py
```

第一轮让 OpenCode 写 `/workspace/plan.md`，随后 `sandbox.pause()` 对 VM 打快
照。脚本用 `Sandbox.connect(sandbox_id)` 恢复，校验 `/workspace/plan.md` 与
OpenCode 数据目录（`/workspace/.opencode/data/storage/`，会话消息存放在这）仍
在，再用 `-c` 续接最近一次会话执行第二轮。沙箱生命周期用 `try/finally` 手动
管理（不用 context manager），避免 pause 后被过早 `kill` 掉。

## 6. 受限出网 + 密钥保险柜（推荐用于共享集群）

```bash
python network_policy.py
```

- 出网默认拒绝，仅放行 LLM host（`OPENCODE_LLM_HOST`）。
- CubeEgress 在链路上把 provider 密钥作为 HTTP 头注入（Anthropic 用
  `x-api-key`，其他用 `Authorization: Bearer`），因此沙箱内 `printenv` 看
  不到真实密钥，只有占位值。
- OpenCode 是 Node.js 包，忽略系统 CA 库。脚本会设置 `NODE_EXTRA_CA_CERTS`
  让 OpenCode 信任 CubeEgress 的拦截 CA；否则 vault 路径会以
  `unable to verify the first certificate` 失败。若镜像里 CA 路径不同，
  可用 `OPENCODE_NODE_EXTRA_CA_CERTS` 覆盖。
- 任何其他目的地都会返回 `403 Forbidden - CubeEgress`。

若 Agent 需要访问额外主机（包镜像源、MCP 服务器等），请增加对应的放行规则，
或把这些依赖预装进模板。

## 7. 宿主端执行器（`sandbox_exec.py`）

如果只是要跑一些不受信任的代码、不需要 OpenCode Agent 自身，可以直接
用 `sandbox_exec.py` CLI 把代码送进一次性 MicroVM。宿主保持干净，每次
调用结束沙箱被销毁（或复用其会话缓存）。

```bash
python sandbox_exec.py --code "print(1+1)"
python sandbox_exec.py --file ./script.py
python sandbox_exec.py --cmd "ls -la /workspace"
python sandbox_exec.py --pip requests --code "import requests; print(requests.__version__)"

# 跨次调用复用同一沙箱，避免冷启动
python sandbox_exec.py --keep-alive --code "state = 42"
python sandbox_exec.py --cmd "echo state still alive"

# 强制重建沙箱
python sandbox_exec.py --reset
```

跨进程缓存使用 `/tmp/cubesandbox_opencode_session_<uid>` 下的会话文件
（`O_NOFOLLOW` + `0600` + `symlink → S_ISREG` 校验），共享主机上的其
他用户无法劫持你下一次的 `Sandbox.connect()`。

## 8. MCP server（`mcp_server.py`）

同一个执行后端也可以通过 newline-delimited JSON-RPC MCP 协议暴露出去，
让任何 MCP 客户端（Claude Desktop、Cursor、Windsurf、VS Code …）都能
在 OpenCode 沙箱里跑不受信任的代码，而不是直接跑在本地。提供 5 个工
具：

| 工具 | 用途 |
| --- | --- |
| `sandbox_run_code` | 在沙箱内执行一段 Python 代码 |
| `sandbox_run_command` | 在沙箱内执行任意 shell 命令 |
| `sandbox_write_file` | 写入文件到沙箱 |
| `sandbox_read_file` | 从沙箱读回文件 |
| `sandbox_reset` | 销毁当前沙箱，下次重新创建 |

把它接入一个 MCP 客户端（以 Claude Desktop 为例）：

```json
{
  "mcpServers": {
    "cubesandbox-opencode": {
      "command": "python3",
      "args": ["/abs/path/to/examples/opencode-integration/mcp_server.py"],
      "env": {
        "E2B_API_URL": "http://<cube-host>:3000",
        "E2B_API_KEY": "<api-key>",
        "CUBE_TEMPLATE_ID": "<template-id>"
      }
    }
  }
}
```

Server 生命周期是进程级的：首次调用时创建一个沙箱，每次调用都刷新
TTL，`atexit` 钩子在 MCP 进程退出时销毁。`sandbox_reset` 是中途强制
重建的唯一入口。

## 9. 宿主 OpenCode + bash 路由插件（`hooks/`）

对于相反的工作流——OpenCode 留在宿主机，但每一次 `bash` 工具调用都
路由到 CubeSandbox——可以安装 `hooks/` 里的 JavaScript 插件：

```bash
cd examples/opencode-integration
python3 -m pip install -r requirements.txt
cp .env.example .env
# 填写 E2B_API_URL 和 CUBE_TEMPLATE_ID

cd hooks
./install.sh
```

安装脚本会把 `cubesandbox-sandbox.js` 复制到
`~/.config/opencode/plugins/`，并在同目录写一个 `package.json` 声明
`"type": "module"`（OpenCode 跑在 Bun 运行时，需要解析 ESM `import`）。
同时仅把白名单内的 `CUBE_*` 配置合并进
`~/.config/opencode/opencode.json`，LLM provider 密钥
（`ANTHROPIC_API_KEY` 等）永远不会被复制进去。

安装后重启 OpenCode。插件的 `tool.execute.before` 钩子拦截 `bash`
调用，对宿主 `python3 sandbox_exec.py --cmd <quoted>` 派生子进程，复
用已缓存的会话沙箱，然后把原本的宿主 shell 命令替换成沙箱的执行结
果。其它工具（`read`、`edit`、`write` 等）继续在宿主上运行——插件
只拦截 `bash`，与 Claude Code PreToolUse 钩子的 bash-only 范围对齐。

卸载：`hooks/install.sh --uninstall`。

## 运行测试

```bash
cd examples/opencode-integration
python3 -m pip install pytest
python3 -m pytest tests -v
```

测试完全离线：不需要 CubeSandbox 集群或 LLM 凭据。`sandbox_exec` 和
`mcp_server` 通过 `unittest.mock` 桩接 e2b SDK 来跑；环境变量相关辅助
用 `monkeypatch`，所以测试顺序不会泄漏状态。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `opencode: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| 权限弹窗卡住整个 run | 未带 `--dangerously-skip-permissions` 标志，且运行会读写/执行命令 | 默认调用已带；若需更严格默认，请在 `opencode.json` 里配置 permissions |
| `unknown flag: --dangerously-skip-permissions` | OpenCode 版本早于 1.17 | 用 `--build-arg OPENCODE_VERSION=1.17.20` 重建镜像，或改用 `OPENCODE_PERMISSION='{"*":"allow"}'` 环境变量 |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 OpenCode 报 `Connection error` / TLS 失败 | OpenCode 是 Node.js 包，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `OPENCODE_NODE_EXTRA_CA_CERTS` 覆盖 |
| 模板创建卡在 `PULLING` | registry 无法被 Cube 节点访问 | 推送到集群可达的 registry，或传入鉴权参数 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |
| `opencode run` 报 `model not found` | `OPENCODE_MODEL` 与 provider 不匹配 | 显式按 provider 设置 `OPENCODE_MODEL`；也可用 OpenCode 的 `provider/model` 简写通过 `-m anthropic/claude-sonnet-4-6` 传入 |

## 参考资料

- 集成指南：[`docs/guide/integrations/opencode.md`](../../docs/zh/guide/integrations/opencode.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 网络 / 出网策略示例：[`examples/network-policy`](../network-policy)
- 凭证保险柜 + 出网管控：[`docs/zh/guide/security-proxy.md`](../../docs/zh/guide/security-proxy.md)
- OpenCode CLI：<https://opencode.ai/>
- OpenCode 文档：<https://opencode.ai/docs/>
