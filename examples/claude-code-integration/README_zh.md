# Claude Code + CubeSandbox

[English README](README.md)

在 CubeSandbox MicroVM 内运行 [Anthropic Claude Code](https://docs.anthropic.com/en/docs/claude-code) —
面向终端的 AI 编码 Agent。Agent 拿到一个 KVM 硬件级隔离的 rootfs，跟宿主机与其他租户完全隔开；
运维侧则拿到一条可审计的出口链路，API Key 永远不进入 VM。

```
┌───────────────────────┐          ┌───────────────────────┐
│  宿主端驱动脚本         │  E2B     │  CubeSandbox MicroVM  │
│  (run_claude.py)       │ 协议     │                       │
│                        │ ────────►│  envd  (:49983)       │
│  Anthropic Key 保留在   │          │  claude CLI (Node 20) │
│  宿主端；或由 CubeEgress│          │  git / python3 / rg   │
│  在出口注入            │          │  /workspace           │
└──────────┬─────────────┘          └───────────┬───────────┘
           │                                    │ HTTPS
           │                                    ▼
           │                             ┌─────────────────┐
           └─── (可选注入规则) ────────►│  CubeEgress     │───► api.anthropic.com
                                         └─────────────────┘
```

## 你能得到什么

| 能力 | CubeSandbox 如何提供 |
|---|---|
| 给 Agent 一个硬件级隔离的 rootfs | KVM MicroVM（Cloud Hypervisor），独立 guest kernel |
| 亚秒级冷启动的干净工作区 | 从 template 快照启动，<60ms |
| 长任务断点续跑 | `sandbox.pause()` + `Sandbox.connect(sandbox_id)` |
| API Key 永远不进入沙箱 | 通过 CubeEgress `inject` 规则在出口挂载 `x-api-key` 头 |
| 出口管控 | 域名白名单，只放行 `api.anthropic.com`（或你自己的网关） |
| 全量 LLM 请求审计 | 每台宿主机 `/data/log/cube-egress/access.jsonl` |

## 目录结构

```
claude-code-integration/
├── README.md                # 英文说明
├── README_zh.md             # 本文件
├── Dockerfile               # cubesandbox-base + Node 20 + @anthropic-ai/claude-code
├── env.example              # 复制为 .env 后填写
├── env_utils.py             # dotenv + envs=... 工具函数
├── requirements.txt         # e2b>=2.4.1, python-dotenv
├── run_claude.py            # 最小一次性任务示例
├── resume_claude.py         # 跨两轮的 pause/resume 示例
└── network_policy.py        # CubeEgress 注入规则（Key 保留在保险柜）
```

## 1. 构建并注册 template

任意支持 Docker 的 x86_64 主机都可以构建镜像；镜像本身只需要放到 Cube 集群能拉取的 registry。

```bash
# 1) 构建
docker build -t claude-code-cube:latest examples/claude-code-integration

# 2) 推到 Cube 集群可访问的 registry
docker tag  claude-code-cube:latest <your-registry>/claude-code-cube:latest
docker push                        <your-registry>/claude-code-cube:latest

# 3) 注册为 Cube template
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

# 4) 等待 READY
cubemastercli tpl watch --job-id <job_id>
```

镜像继承 `cubesandbox-base`，所以 envd 已经在 `:49983` 监听。Claude Code 位于
`/usr/bin/claude`，状态目录在 `/root/.claude/`；镜像里没有任何用户敏感信息。

## 2. 配置驱动脚本

```bash
cd examples/claude-code-integration
cp env.example .env      # 填 E2B_API_URL、CUBE_TEMPLATE_ID、ANTHROPIC_API_KEY
pip install -r requirements.txt
```

| 变量 | 流向 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 任意非空字符串即可 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 步骤 1 中获得 |
| `ANTHROPIC_API_KEY` | 通过 `envs=...` 逐条命令注入 | 若走 CubeEgress 注入模式可省 |
| `ANTHROPIC_MODEL` | 可选，例如 `claude-sonnet-4-5` | 直接透传给 `claude` |
| `ANTHROPIC_BASE_URL` | 可选：Anthropic 兼容网关 | 例如 Bedrock 代理 |
| `CLAUDE_CODE_USE_BEDROCK` | 可选，`true` 走 AWS Bedrock | 需在沙箱 exec env 中提供 AWS 凭证；**与 Vertex 互斥** |
| `CLAUDE_CODE_USE_VERTEX` | 可选，`true` 走 Google Vertex AI | 需在沙箱内提供 `GOOGLE_APPLICATION_CREDENTIALS`；**与 Bedrock 互斥** |

## 3. 单次任务

```bash
python run_claude.py --prompt "Create hello.py that prints 'Hello from CubeSandbox!' and run it."
```

脚本内部：

1. 从 Claude Code template 拉起沙箱；
2. `claude --version` 预检，快速识别 template 是否过期；
3. 在 `/workspace` 里执行 `claude --print --allowedTools 'Bash(npm:*),Bash(node:*),Bash(python3:*),Edit,Write,Read' -- <prompt>`，
   API Key 通过 `envs=...` 逐条命令下发；
4. stdout / stderr 实时透传回宿主端；
5. 打印 `/workspace` 的目录列表，便于验收 Agent 产出的文件。

添加 `--stream-json` 会启用 `--output-format stream-json`（Claude Code 每一步输出为 JSON 事件，
便于外部编排）。用 `--seed ./my_project.py` 可以在 Agent 运行前把宿主端文件上传到工作区。

## 4. 使用 pause/resume 支撑长任务

```bash
python resume_claude.py
```

Demo 会先运行一段 Claude Code，然后调用 `sandbox.pause()`：VM 快照落盘，资源释放；之后
再 `Sandbox.connect(sandbox_id)` 恢复，rootfs、`/workspace` 里的文件、Claude Code 落盘状态
（`~/.claude/`）都完好保留。适合以下场景：

- 长时间重构任务，中途离场再回来继续；
- 交互式会话希望在宿主机维护窗口内保留状态；
- 从同一基线 fork 出 N 个变体。

E2B 协议中的 `pause` 直接对应 CubeSandbox 的 [快照 / 克隆 / 回滚引擎](../../docs/zh/guide/snapshot-rollback-clone.md)。

## 5. 密钥保险柜模式（多租集群推荐）

`network_policy.py` 展示 **API Key 永远不进入 VM** 的最佳实践。沙箱创建时携带一组
CubeEgress 规则：

- 默认拒绝所有出口；
- 放行 `https://api.anthropic.com`；
- 在出口注入 `x-api-key: sk-ant-...` 与 `anthropic-version: 2023-06-01` 头。

```bash
python network_policy.py
```

沙箱内 `printenv | grep ANTHROPIC_API_KEY` 什么都看不到；CLI 依然能通过认证，因为
CubeEgress 在沙箱发出裸请求之后才把 header 挂上去。所有请求都会写入宿主机
`/data/log/cube-egress/access.jsonl`，包含规则名、沙箱 IP、method、path、耗时、TLS 结果。

如果你使用 Bedrock、Vertex 或自建 Anthropic 网关：把 `ANTHROPIC_BASE_URL` 指向网关，规则里
的 `sni` / `host` 也换成对应域名。完整语法参见 [安全代理指南](../../docs/zh/guide/security-proxy.md)。

## 6. 最佳实践

**出口策略**：默认拒绝，只按需放行 Claude Code 需要的域名。通常只需要
`api.anthropic.com`；如果任务里会 `npm install`，再加上 `registry.npmjs.org`。

**工具白名单**：明确传 `--allowedTools`；只有在你完全接受沙箱风险时才用
`--dangerously-skip-permissions`。白名单可以省掉一次交互往返。

**用快照当 commit**：Agent 完成一个自洽的小步骤后 `sandbox.pause()` 就是一个可回滚的
checkpoint。下一步失败时可以 `Sandbox.connect(sid)` 回到上一个快照重试。

**并发 Agent**：一个会话一个沙箱。CubeSandbox `<60ms` 冷启动让 N 路并行开销可忽略；硬件
隔离保证一个 Agent 出错不会影响其他 Agent。

**工作目录**：只把 `/workspace` 当用户产物目录。`/root/.claude/` 是 Agent 的短期状态，
丢了也只是丢短期记忆，不影响你已经产出的代码。

## 7. 常见问题

| 现象 | 可能原因 | 解决 |
|---|---|---|
| 预检报 `claude: command not found` | Template 有问题 | 检查 `CUBE_TEMPLATE_ID`；重建镜像；镜像内 Node ≥ 18 |
| Claude Code 报 `Invalid API key` | Key 没进沙箱，也没配 CubeEgress | 参考 §5，或把 Key 加到 `envs=...` |
| 出口命中 `403 Forbidden — CubeEgress` | 默认拒绝但没有 allow 规则 | 增加 `Match(sni="api.anthropic.com")`（或你的网关） |
| Template 就绪探测失败 | 基础镜像没有 envd | 确保 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `Cannot find /workspace` | 上层镜像换了 `WORKDIR` | 传 `--workspace /some/dir` 并先 `mkdir -p` |
| Agent 卡在 `Bash(...)` 提示 | 没配 `--allowedTools`，且没有 TTY | 使用 `--print` + `--allowedTools`（两个脚本都是这么做的） |
| 访问 `api.anthropic.com` SSL 握手失败 | 沙箱不信任 CubeEgress 证书 | Template 需以 `--with-cube-ca=true`（默认）构建，或设置 `SSL_CERT_FILE` |

## 8. 相关文档

- [集成指南（中文）](../../docs/zh/guide/integrations/claude-code.md)
- [Integration guide (English)](../../docs/guide/integrations/claude-code.md)
- [自带镜像（envd）](../../docs/zh/guide/tutorials/bring-your-own-image.md)
- [从 OCI 镜像创建 Template](../../docs/zh/guide/tutorials/template-from-image.md)
- [快照 · 克隆 · 回滚](../../docs/zh/guide/snapshot-rollback-clone.md)
- [安全代理 · 密钥保险柜](../../docs/zh/guide/security-proxy.md)
- [Claude Code CLI 参考](https://docs.anthropic.com/en/docs/claude-code/cli-reference)
