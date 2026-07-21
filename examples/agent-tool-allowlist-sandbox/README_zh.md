# Agent 工具白名单沙箱

[English](README.md)

在 Cube Sandbox MicroVM 内执行 **白名单内的 Agent 工具命令**，回传 stdout /
小工件；当 Agent 请求 **非白名单** 命令时在宿主机侧 **快速失败**。

这不是通用代码解释器、Jupyter 运行时，也不是 OpenClaw / OpenAI Agents 整套托管。
白名单门控放在 **宿主机（SDK 调用方）**，叙事保持诚实：MicroVM 仍负责隔离执行，
示例展示 Agent 应如何在发往沙箱前拒绝任意工具。

## 1. 与现有 example / #645 PR 的差异

**一句话：** 不是又一个语言运行时或完整 Agent 托管，而是 **Agent 工具调用的宿主机 argv 白名单门控** → MicroVM 执行 → 产物回传；非白名单 **快速失败且不创建沙箱**。

### 仓内已有 example

| 示例 | 关注点 | 「白名单」含义 |
|------|--------|----------------|
| [`code-sandbox-quickstart`](../code-sandbox-quickstart) | 任意 `run_code` / `commands.run` | 无（任意命令） |
| [`openai-agents-code-interpreter`](../openai-agents-code-interpreter) | LLM + 数据分析 / 代码解释器 | 不适用 |
| [`openclaw-integration`](../openclaw-integration) / [`openai-agents-example`](../openai-agents-example) | 完整 Agent 托管 / 编排 | 不适用 |
| [`network-policy`](../network-policy) / `network_allowlist.py` | **出口 CIDR** 放行/拒绝 | 网络目的地址 |
| **本示例** | **工具 argv** 放行/拒绝 + 产物读回 | 首个 argv（二进制名） |

### 与 [#645](https://github.com/TencentCloud/CubeSandbox/issues/645) 社区 PR 主题对照（刻意不重叠）

| 主题（举例） | 那些 PR 在做什么 | 本示例怎么避开 |
|--------------|------------------|----------------|
| 语言 / Web 运行时（如 Node #732、Go/Rust/Java #735/#755/#782、C++ #876、Ruby #926） | 新语言镜像 + 在沙箱里跑该栈 | 基于官方 `sandbox-code` 的薄 Dockerfile — 不新增语言栈 |
| 解释器 / 笔记本（如 Jupyter ML #1025、RCA interpreter #745） | 任意或分析向代码执行 | 只做工具命令子集；非白名单拒绝 |
| Agent 框架（如 LangGraph #710） | 端到端 Agent 编排 | 只提供门控模式，不做框架集成 |
| 平台能力演示（egress / DB / 快照，如 #748、#979、#1004、#738） | CIDR 策略、有状态服务、快照缓存 | 做的是 **argv** 白名单（不是 CIDR）；示例保持最小 |

提 PR 前再搜一遍 open 的 #645 PR 标题是否出现 `allowlist` / `tool-allowlist`，避免同向撞车。

## 2. 前置条件

- 已部署的 Cube Sandbox（见 [开发环境](../../docs/zh/guide/dev-environment.md)）
- Python 3.8+

```bash
pip install -r requirements.txt
```

## 3. 创建模板

两条路径。要满足 #645「可构建模板」门槛时优先走 **路径 B**。路径 A 与
[`code-sandbox-quickstart`](../code-sandbox-quickstart/README_zh.md) 完全一致。

### 路径 A — 直接复用官方 `sandbox-code`（最快）

```bash
# 国内镜像
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999

# 国际镜像（如在海外）
# cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

### 路径 B — 构建本示例 Dockerfile 再注册模板

基于 `sandbox-code` 的薄封装（见 [`Dockerfile`](./Dockerfile)）：设置
`TOOL_ALLOWLIST_SANDBOX=1`，写入 `/etc/cube-sandbox/tool-allowlist.txt`。
**不**新增语言运行时。

```bash
# 在本目录下
docker build -t agent-tool-allowlist-sandbox:latest .

# 可选：打标签并推到 CubeMaster 可拉取的仓库
# docker tag agent-tool-allowlist-sandbox:latest <your-registry>/agent-tool-allowlist-sandbox:latest
# docker push <your-registry>/agent-tool-allowlist-sandbox:latest

# create-from-image 参数与 quickstart 相同（端口 / probe 不变）
cubemastercli tpl create-from-image \
  --image agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

国际基础镜像覆写：

```bash
docker build \
  --build-arg SANDBOX_CODE_IMAGE=cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  -t agent-tool-allowlist-sandbox:latest .
```

任一路记下输出的 `template_id`。
## 4. 配置环境变量

```bash
cp .env.example .env
# 填写 E2B_API_URL 与 CUBE_TEMPLATE_ID
```

官方 QEMU `dev-env` 下，宿主机转发 API 一般为 `http://127.0.0.1:13000`。
完整 E2B 命令流量还需要 `*.cube.app` DNS（guest 内 CoreDNS）。建议在
**开发虚机内**运行本示例，或在宿主机配置泛域名解析到 CubeProxy 转发端口
（宿主机 `11080`/`11443`）。

## 5. 运行

### 本地验证（默认不需要沙箱）

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
# Unix: source .venv/bin/activate
pip install -r requirements.txt
python verify_local.py
```

会跑单测、`run_denied.py`、Dockerfile↔`allowlist.py` 漂移检查，以及（若本地有镜像）
Docker 标记检查。若已设置 `E2B_API_URL` 与 `CUBE_TEMPLATE_ID`，还会尝试
`run_allowlisted.py`。

> 即使 `http://127.0.0.1:13000/health` 正常，宿主机若没有 `*.cube.app` DNS，
> allow 路径仍可能报 `getaddrinfo failed`。建议在 **开发虚机内** 跑 allow 脚本（见 §4）。

### 白名单工具（成功）

```bash
python run_allowlisted.py
```

预期：

```text
agent-tool-allowlist-ok
artifact: artifact-ok
```

### 非白名单工具（宿主机必须失败）

```bash
python run_denied.py
```

预期：

```text
denied_as_expected: command not on tool allowlist: 'bash' ...
```

拒绝路径不会创建沙箱。

## 6. 默认白名单

见 `allowlist.py` 中的 `DEFAULT_ALLOWED_BINARIES`。演示仅开放少量只读/汇报类工具
（`echo`、`uname`、`ls`、`cat`、`python3` 等），拒绝路径形式二进制（`/bin/bash`）
以及 `bash` / `curl` 等。

按你的 Agent 场景收紧或扩展，并在代码评审中保持显式。

## 7. 限制（上线前请阅读）

本示例是 **宿主机侧 defense-in-depth**，不能替代 Cube 平台层强制。对照表
（egress 事实来自 [`network-policy` README §6](../network-policy/README.md)）：

| 层次 | 管什么 | 强制位置 | 沙箱内能否绕过 |
|------|--------|----------|----------------|
| **本示例**（`assert_allowlisted`） | 工具 **argv**（首个二进制名） | 宿主机 SDK 调用方（Python） | 不适用——跳过门控直接调 API 即可绕过 |
| **出口策略**（`allow_out` / `deny_out` / `allow_internet_access`） | 网络 **目的 CIDR** | Cubelet **tap** 网络层（出 VM 前） | **不能**——内核级；沙箱内无法绕过 |
| Guest seccomp / AppArmor | 系统调用 / MAC（若配置） | Guest 内核 / LSM | 本示例不演示 |

- 绕过 `assert_allowlisted` 的调用方仍可向 API 发送任意命令；真实 Agent 需配合网络策略与最小权限凭证。
- 路径 B 镜像内的 `/etc/cube-sandbox/tool-allowlist.txt` 仅为信息标记；本演示仍以宿主机门控为准。
- 不做未授权扫描或 exploit 类工具演示。

## 8. 目录结构

```text
agent-tool-allowlist-sandbox/
├── README.md
├── README_zh.md
├── Dockerfile             # 基于官方 sandbox-code 的薄封装
├── allowlist.py           # 宿主机 argv 门控（唯一真相源）
├── allowlist_sync.py      # 由 allowlist.py 生成 guest 清单正文
├── verify_local.py        # 本地验证门控
├── run_allowlisted.py     # 放行路径 + 产物读回
├── run_denied.py          # 拒绝路径（不建沙箱）
├── test_allowlist.py      # 本地单测（无需沙箱）
├── env_utils.py
├── requirements.txt
└── .env.example
```
