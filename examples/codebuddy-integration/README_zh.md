# CodeBuddy + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 内运行 [腾讯云 CodeBuddy Code CLI](https://www.codebuddy.ai/docs/cli/README)
（面向终端的 AI 编码 Agent）。Agent 在一个隔离、可复现的沙箱内编辑文件、执行命令并访问 LLM API。

本示例包含：

- 一个 `Dockerfile`：在 CubeSandbox 基础镜像上叠加 Node.js 20 与 CodeBuddy CLI（envd 已监听 `:49983`）。
- `run_codebuddy.py`：在 `/workspace` 内的一次性无交互运行。
- `resume_codebuddy.py`：跨两轮的 pause/resume，证明 `/workspace` 与 CodeBuddy 状态目录（`/workspace/.codebuddy`）在快照后仍存在。
- `network_policy.py`：默认拒绝出网的策略，由 CubeEgress 在链路上注入 API Key，密钥不进入 VM。
- `env_utils.py`、`_codebuddy_common.py`、`.env.example`、`requirements.txt`。

## 目录结构

```
codebuddy-integration/
├── Dockerfile            # CubeSandbox 模板镜像（Node.js + CodeBuddy CLI）
├── .env.example          # 复制为 .env 并填写
├── .gitignore
├── requirements.txt      # 宿主端驱动依赖（e2b、cubesandbox、python-dotenv）
├── env_utils.py          # .env 加载、provider key、CodeBuddy 命令构造
├── _codebuddy_common.py  # 共享的沙箱命令辅助（run/ensure/id）
├── run_codebuddy.py      # 一次性 CodeBuddy 任务
├── resume_codebuddy.py   # pause / resume 会话持久化
├── network_policy.py     # 默认拒绝出网 + 链路上注入密钥
├── README.md             # 英文文档
└── README_zh.md          # 中文文档（本文件）
```

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- 一个 CodeBuddy Code 账号（或自定义上游 API Key — Anthropic、OpenAI、DeepSeek、Google Gemini 等）。见 `.env.example`。
- Python 3.10+（宿主端驱动脚本）。

## 1. 构建模板镜像

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/codebuddy-cube:latest \
  examples/codebuddy-integration
docker push <your-registry>/codebuddy-cube:latest
```

镜像会安装 `@tencent-ai/codebuddy-code`，以及 `git`、`python3`、`ripgrep`、`jq`，并清理
apt/npm 缓存。CodeBuddy 版本通过 `--build-arg CODEBUDDY_VERSION=x.y.z` 固定。

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/codebuddy-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务变为 `READY` 后记下 `template_id`。

## 3. 配置宿主端驱动

```bash
cd examples/codebuddy-integration
cp .env.example .env
# 填写 CUBE_API_URL、CUBE_TEMPLATE_ID、CODEBUDDY_INTERNET_ENVIRONMENT 以及你的密钥
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `CUBE_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `CUBE_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `CODEBUDDY_INTERNET_ENVIRONMENT` | CodeBuddy CLI | `io`（默认，国际版）、`internal`（国内）、`ioa`（腾讯企业版） |
| `CODEBUDDY_MODEL` | CodeBuddy CLI | 对应 provider 的模型 id |
| `CODEBUDDY_API_KEY` / `ANTHROPIC_API_KEY` / ... | `envs=...`（直连）或 CubeEgress 注入（vault） | provider 密钥 |
| `CODEBUDDY_BASE_URL` / `ANTHROPIC_BASE_URL` | CodeBuddy CLI | 自定义上游端点（如通过 Anthropic 兼容网关接 DeepSeek） |
| `CODEBUDDY_LLM_HOST` | `network_policy.py` | 放行的 LLM API host，默认从 `*_BASE_URL` 解析或取 provider 默认 |

## 4. 一次性运行（直连注入密钥）

```bash
python run_codebuddy.py --prompt "创建 hello.py 打印 'Hello from CubeSandbox' 并运行它。"
```

CodeBuddy 以无交互模式启动：`-p` 让它处理完 prompt 即退出（不进入交互 TUI），配合 `-y`
（`--dangerously-skip-permissions`，任何会读写文件或执行命令的非交互运行都必须带，否则
CLI 会卡在无法在 exec 信道回答的权限弹窗上）。密钥通过
`sandbox.commands.run(..., envs=...)` 逐命令传入，只在该命令执行期间存在，不会写入 VM 内的
持久文件。

> **安全：** 直连方式出网是放开的，Agent 被攻破可能外泄注入的密钥。共享集群请用保险柜方式
> （第 6 步）：默认拒绝出网 + 链路上注入密钥。

## 5. pause / resume（会话持久化）

```bash
python resume_codebuddy.py
```

第一轮让 CodeBuddy 写 `/workspace/plan.md`，随后 `sandbox.pause()` 对 VM 打快照。脚本用
`Sandbox.connect(sandbox_id)` 恢复，校验 `/workspace/plan.md` 与 CodeBuddy 状态目录
（`/workspace/.codebuddy/projects/...`）仍在，再用 `-c` 续接最近一次会话执行第二轮。沙箱生命周期
用 `try/finally` 手动管理（不用 context manager），避免 pause 后被过早 `kill` 掉。

## 6. 受限出网 + 密钥保险柜（推荐用于共享集群）

```bash
python network_policy.py
```

- 出网默认拒绝，仅放行 LLM host（`CODEBUDDY_LLM_HOST`）。
- CubeEgress 在链路上把 provider 密钥作为 HTTP 头注入（Anthropic 用 `x-api-key`，其他用
  `Authorization: Bearer`），因此沙箱内 `printenv` 看不到真实密钥，只有占位值。
- CodeBuddy 是 Node.js 包，忽略系统 CA 库。脚本会设置 `NODE_EXTRA_CA_CERTS` 让 CodeBuddy
  信任 CubeEgress 的拦截 CA；否则 vault 路径会以 `unable to verify the first certificate`
  失败。若镜像里 CA 路径不同，可用 `CODEBUDDY_NODE_EXTRA_CA_CERTS` 覆盖。
- 任何其他目的地都会返回 `403 Forbidden - CubeEgress`。

若 Agent 需要访问额外主机（包镜像源、MCP 服务器等），请增加对应的放行规则，或把这些依赖预
装进模板。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `codebuddy: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| 权限弹窗卡住整个 run | 忘了在会读写/执行命令的运行上加 `-y` / `--dangerously-skip-permissions` | 默认调用已带 `-y`；若需要更严格默认，请在 `settings.json` 里配置 permissions |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 CodeBuddy 报 `Connection error` / TLS 失败 | CodeBuddy 是 Node.js 包，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `CODEBUDDY_NODE_EXTRA_CA_CERTS` 覆盖 |
| 模板创建卡在 `PULLING` | registry 无法被 Cube 节点访问 | 推送到集群可达的 registry，或传入鉴权参数 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |
| 启动时弹出登录浏览器界面卡住 | 默认模式为交互式；`-p` 要求预设 API Key | 设置 `CODEBUDDY_API_KEY`（或对应的 provider 环境变量）—— 非交互模式不会回落到浏览器登录 |

## 参考

- 集成指南：[`docs/guide/integrations/codebuddy.md`](../../docs/zh/guide/integrations/codebuddy.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 网络 / 出网策略示例：[`examples/network-policy`](../network-policy)
- 凭证保险柜 + 出网管控：[`docs/zh/guide/security-proxy.md`](../../docs/zh/guide/security-proxy.md)
- CodeBuddy Code CLI：<https://www.codebuddy.ai/docs/cli/README>
