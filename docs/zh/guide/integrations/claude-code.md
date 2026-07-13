---
title: Claude Code 集成指南
author: shsaihdsaiudh
date: 2026-07-06
tags:
  - integration
  - claude-code
  - coding-agent
lang: zh-CN
---

# Claude Code

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) 是 Anthropic 开发的终端型 AI 编码 Agent。它能在终端环境中运行代码、编辑文件、执行命令——天然适合以 CubeSandbox 作为执行后端，实现隔离、可复现的开发工作流。

本指南同时介绍在 CubeSandbox MicroVM 中运行 Claude Code，以及让 Claude Code 留在宿主机、只沙箱化其 Bash 工具调用的方案。

## 架构

```
宿主机（编排端）
    │  e2b-code-interpreter / E2B SDK
    │  Python 脚本 (run_claude_code.py, network_policy.py 等)
    ▼
CubeAPI (:3000) ──► CubeMaster ──► Cubelet ──► KVM MicroVM
                                                    │
                                                Claude Code CLI (Node.js)
                                                    │  -p / --print (无头模式)
                                                    │  ANTHROPIC_AUTH_TOKEN
                                                    │  ANTHROPIC_BASE_URL
                                                    ▼
                                                Anthropic 协议 LLM API
```

下方模式 1-3 会在 MicroVM 内运行 Claude Code 本身；模式 4 使用宿主机侧 `PreToolUse` Hook：

```
Claude Code（宿主机）
    |-- Read / Write / Edit ----------> 宿主机项目
    `-- Bash --> PreToolUse Hook --> CubeAPI --> 可复用 MicroVM
                                                `-- 只读挂载项目目录
```

## 环境准备

- 运行中的 [CubeSandbox 部署](/zh/guide/quickstart)
- Python 3.9+ 和 `e2b-code-interpreter`
- CubeSandbox 代码模板（见下方[模板创建](#模板创建)）
- LLM 提供商的 API 密钥

## 模板创建

### 方案一：构建自定义镜像（推荐）

创建扩展 CubeSandbox 基础镜像的 Dockerfile，预装 Claude Code：

```dockerfile
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

ENV NPM_CONFIG_PREFIX=/usr/local

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && npm install -g @anthropic-ai/claude-code
```

```bash
docker build -t claude-code-sandbox:v1 .
docker push <your-registry>/claude-code-sandbox:v1

cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

### 方案二：使用 sandbox-code 运行时安装

::: warning 启动延迟
在沙箱创建时安装 Node.js + Claude Code 会增加约 30-60 秒的冷启动时间。生产环境建议使用方案一。
:::

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

## 环境变量配置

Claude Code 在沙箱中需要以下环境变量：

| 变量 | 说明 | 示例 |
|------|------|------|
| `CC_PROVIDER` | LLM 提供商：`deepseek` 或 `anthropic` | `deepseek` |
| `ANTHROPIC_AUTH_TOKEN` | API 密钥（DeepSeek / Anthropic） | `sk-a1b2c3d4...`（DeepSeek）或 `sk-ant-...`（Anthropic） |
| `ANTHROPIC_BASE_URL` | API 端点地址（DeepSeek / Anthropic） | `https://api.deepseek.com/anthropic` |
| `CC_MODEL` | 集成脚本选择的模型 | `deepseek-v4-pro` |
设置 `CC_PROVIDER=anthropic` 切换到 Anthropic API。Claude Code 使用 Anthropic 协议，仅设置 `OPENAI_API_KEY` 和 `OPENAI_BASE_URL` 无法直接使用 OpenAI 兼容端点。

::: tip 使用 DeepSeek？
设置 `ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic`，模型名如 `deepseek-v4-pro`。
:::

## 使用模式

### 1. 单次执行

运行单个提示词并回收结果，适合 CI/CD 和代码生成场景。

```bash
python run_claude_code.py "写一个对整数列表排序的 Python 函数"
```

**工作流程：**

1. 从模板创建沙箱
2. 注入环境变量
3. 运行 `claude --print '<prompt>'`
4. 输出结果并销毁沙箱

### 2. 会话持久化（暂停/恢复）

启动会话、暂停沙箱、需要时恢复。

```bash
# 启动会话
python resume_claude_code.py "搭建一个 FastAPI 项目骨架"

# 输出：Sandbox paused. 稍后恢复：
#   python resume_claude_code.py --resume-from <sandbox-id>

# 恢复并继续
python resume_claude_code.py --resume-from <sandbox-id> "添加 /health 端点"
```

**工作流程：**

1. 创建沙箱，运行 Claude Code，调用 `sandbox.pause()`
2. 整个 VM 状态（内存、磁盘、进程树）被快照保存
3. 之后 `Sandbox.connect(sandbox_id)` 恢复 VM
4. Claude Code 在完整上下文中恢复，所有对话和文件修改均保留

### 3. 安全出口与密钥注入

沙箱**默认无法访问互联网**。仅 LLM API 地址通过 CubeEgress 放行，真实凭证在线路上注入。

```bash
# 生产环境（默认）：沙箱无网络，需使用预装镜像
python network_policy.py "分析这段代码的安全性"

# 首次安装：允许联网以在线安装 Claude Code
python network_policy.py --allow-internet "分析这段代码的安全性"
```

**工作流程：**

1. 沙箱以 `allow_internet_access=False` 创建（默认；首次安装用 `--allow-internet`）
2. CubeEgress 规则仅放行 LLM API 地址（如 `api.deepseek.com`）
3. 沙箱内设置占位凭证 `ANTHROPIC_AUTH_TOKEN=sk-placeholder`
4. CubeEgress 拦截 TLS 流量，将 `Authorization: Bearer sk-placeholder` 替换为真实密钥
5. 沙箱永远看不到真实密钥——即使被攻破也无法泄露

### 4. 宿主机 Claude Code + PreToolUse Hook

当 Claude Code 需要在宿主机保持交互、但其 `Bash` 工具调用需要进入 CubeSandbox 时，可以使用该方案。在示例目录中执行：

```bash
python3 -m pip install -r requirements.txt
cp .env.example .env
# 在 .env 中设置 CUBE_API_URL 和 CUBE_TEMPLATE_ID

cd hooks
./install.sh
```

安装后重启 Claude Code。安装脚本会把 `Bash` 匹配器合并进 `~/.claude/settings.json`，并且只把白名单内的 `CUBE_*` 配置写入 Hook 配置，不会复制 LLM 提供商的 API 密钥。

每个 Claude Code `session_id` 复用一个沙箱，映射默认保存在 `~/.cache/cubesandbox-hook/`。Hook 还会在多次 Bash 调用间保留沙箱 Shell 的工作目录和导出的环境变量。

第一次调用时，Hook 可以请求把 Claude Code 项目目录以相同路径**只读**挂载进沙箱。需要把项目根目录加入 CubeMaster 的宿主机挂载允许列表：

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

`hostPath` 在调度到的 Cubelet 节点上解析，而不是在运行 Claude Code 的机器上解析。只有 Claude Code 与 Cubelet 位于同一节点，或项目已通过共享存储/同步机制存在于每个候选 Cubelet 的相同绝对路径时，才能得到一致文件视图。Hook 不会把本地项目上传或同步到远端部署；不要直接放行只存在于客户端、但可能在 Cubelet 上指向其他数据的路径。

该 Hook 只覆盖 Claude Code 的 `Bash` 工具。`Read`、`Write`、`Edit` 仍然访问宿主机，沙箱命令也不能通过只读挂载写入项目文件或构建产物。如果 CubeMaster 拒绝挂载，命令会降级到无挂载沙箱中执行；Bash 仍然隔离，但无法与宿主机文件工具保持一致的文件视图。

开始无关任务前或卸载前，可以重置指定会话：

```bash
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

cd hooks
./install.sh --uninstall
```

## 提供商支持

| 提供商 | `ANTHROPIC_BASE_URL` | 默认模型 |
|--------|---------------------|----------|
| DeepSeek | `https://api.deepseek.com/anthropic` | `deepseek-v4-pro` |
| Anthropic | `https://api.anthropic.com` | `claude-sonnet-4-6` |

通过 `.env` 文件配置：

```bash
# .env
CC_PROVIDER=deepseek
ANTHROPIC_AUTH_TOKEN=sk-...
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
CC_MODEL=deepseek-v4-pro
```

## 最佳实践

### 隔离开发

```
                   ┌─────────────────────────┐
                   │  CubeSandbox MicroVM     │
                   │                         │
  宿主机            │  /workspace/            │
  git clone ──────►│  ├── src/               │
                   │  ├── tests/             │
                   │  └── ...                │
                   │                         │
                   │  Claude Code 在此编辑    │
                   │  （安全、可丢弃）         │
                   └─────────────────────────┘
```

### 利用快照实现断点续跑

1. 用 `resume_claude_code.py` 启动 Claude Code
2. 定期暂停：`sandbox.pause()` 创建时间点快照
3. 如果出错，回滚到最近的快照
4. 从快照恢复继续执行

### 代码执行与结果回收

```
编排端
    │ 1. 创建沙箱
    ▼
沙箱（Claude Code 生成代码）
    │ 2. Claude Code 写入 solution.py
    │ 3. 执行：python solution.py
    │ 4. 通过 sandbox.files 读取输出
    ▼
宿主机（回收结果，销毁沙箱）
```

## 常见问题

### `claude: command not found`

模板中未安装 Claude Code。使用方案一（Dockerfile）重建，或运行时安装（方案二）。

### `ANTHROPIC_AUTH_TOKEN not set`

沙箱未配置 API 密钥。检查 `.env` 文件，确认 `build_claude_env()` 返回预期值。

### SSL 证书错误

如果通过 HTTPS 连接 CubeProxy：

```bash
# 在沙箱中
export SSL_CERT_FILE=/usr/local/share/ca-certificates/cube-ca.crt
export NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/cube-ca.crt
```

### 连接 LLM API 超时

沙箱无法访问 LLM API。检查：
1. 网络策略是否放行目标地址（参考 `network_policy.py`）
2. API 地址在沙箱内是否能解析
3. CubeEgress 规则是否正确应用

### 模板未找到

```bash
cubemastercli tpl list
```

确认 `CUBE_TEMPLATE_ID` 对应 `STATUS: READY` 的模板。

## 示例仓库

完整可运行示例见 [`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)，包含：

- `Dockerfile` — 构建预装 Claude Code 的沙箱镜像
- `run_claude_code.py` — 单次执行
- `resume_claude_code.py` — 暂停/恢复会话
- `network_policy.py` — 安全出口与密钥注入
- `env_utils.py` — 环境变量与凭证管理
- `hooks/` — 宿主机侧 `PreToolUse` Hook、执行器与安装脚本
- `tests/` — 共用工具、MCP 处理与 Hook 生命周期测试
