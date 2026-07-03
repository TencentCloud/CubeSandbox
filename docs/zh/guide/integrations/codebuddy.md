---
title: CodeBuddy CLI 集成指南
author: Mizup79
date: 2026-07-03
tags:
  - integration
  - codebuddy
  - cli
lang: zh-CN
---

# CodeBuddy CLI 集成指南

## 集成目标与版本

**CodeBuddy CLI** 是腾讯云推出的 AI 编程助手命令行工具。它通过自然语言对话和工具调用能力，帮助开发者理解、重构和生成代码。

- **npm 包名**：`@tencent-ai/codebuddy-code`
- **命令**：`codebuddy` / `cbc`
- **测试版本**：v2.110.0（2026 年 7 月）
- **运行时**：Node.js v18+（推荐 v22 LTS）
- **架构**：单体 Node.js 进程（非客户端/服务器架构）
- **运行模式**：
  - **原生沙箱**（`codebuddy --sandbox <url>`）— CodeBuddy 在主机运行；工具调用路由到 CubeSandbox MicroVM。推荐个人使用，无需企业账户。
  - **沙箱内执行**（`codebuddy -p`）—— 运行单次提示后退出，适用于 CI/CD 流水线
  - **HTTP API**（`codebuddy --serve`）—— 启动 REST 服务器，供交互式消费
  - **SDK**（`@tencent-ai/agent-sdk`）—— 从 Node.js 进行编程控制
- **认证方式**：`CODEBUDDY_API_KEY` 环境变量或交互式浏览器 OAuth
- **官方文档**：[https://www.codebuddy.ai/docs/cli/overview](https://www.codebuddy.ai/docs/cli/overview)

下图展示了 CodeBuddy 在 CubeSandbox MicroVM 内的运行方式：

```
用户 / CI 流水线
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API（经 CubeEgress 出口）
```

## 前置条件

- **CubeSandbox 部署**——CubeAPI 必须可访问，地址为 `http://<host>:3000`
- **Docker**——用于构建模板镜像（仅沙箱内/HTTP API 模式需要）
- **`cubemastercli` CLI 工具**——随 CubeSandbox 安装
- **Python 3.10+** 及 `e2b-code-interpreter` SDK（`pip install e2b-code-interpreter`）
- **CodeBuddy API 密钥**——在 [https://www.codebuddy.ai/profile/keys](https://www.codebuddy.ai/profile/keys) 生成
- **Node.js v22 LTS**——仅在沙箱外本地测试 CodeBuddy 时需要

### 必需的环境变量

| 变量名 | 说明 | 示例 |
|--------|------|------|
| `E2B_API_URL` | CubeAPI 地址 | `http://192.168.1.100:3000` |
| `E2B_API_KEY` | CubeAPI 认证密钥（任意非空字符串） | `e2b_000000` |
| `CUBE_TEMPLATE_ID` | CodeBuddy 沙箱模板 ID | （由 `cubemastercli` 输出） |
| `CODEBUDDY_API_KEY` | CodeBuddy API 密钥 | `ck_xxxxxxxx` |

## 架构概览

CubeSandbox 支持两种集成模式。两种模式使用相同的模板镜像，
区别在于 **codebuddy 在哪里运行**。

### 原生沙箱模式（推荐）

CodeBuddy 在**你的主机上**运行（已登录），工具调用（Bash、Read、Write）
通过 `--sandbox` 标志路由到沙箱。LLM API 调用通过本地网络直连。

```
用户 / CI 流水线
    │
    ▼
codebuddy CLI (主机本地，已登录)
    │
    ├──► LLM API (主机网络直连)
    │
    └──► 工具调用通过 --sandbox 路由
              │
              ▼
         CubeAPI (port 3000)
              │
              ▼
    CubeMaster ──► Cubelet ──► KVM MicroVM
                                   │
                               envd (PID 1)
                                   │
                               执行工具命令
                                   │
                               结果返回给 codebuddy
```

**认证要求**：仅需 `CODEBUDDY_API_KEY`。无需企业账号。

### 沙箱内模式

CodeBuddy 在 **MicroVM 内部**运行，完全隔离。一切——codebuddy、
工具执行、文件访问——都在沙箱内完成。LLM API 调用通过 CubeEgress 出口，
便于网络管控。

```
用户 / CI 流水线
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API (经 CubeEgress 出口)
```

**认证要求**：`CODEBUDDY_API_KEY` + `CODEBUDDY_AUTH_TOKEN`（企业/CI 账号）。沙箱基础设施已验证，但缺少该凭证，对话环节未能完成验证。

## 我应该使用哪种模式？

| 使用场景 | 模式 | 需要 AUTH_TOKEN？ |
|----------|------|-------------------|
| 快速测试、个人使用 | 原生沙箱 | 不需要——只需 `CODEBUDDY_API_KEY` |
| CI/CD 流水线、企业使用 | 沙箱内 | 需要——需 `CODEBUDDY_AUTH_TOKEN` |
| HTTP API 服务 | HTTP API | 需要——需 `CODEBUDDY_AUTH_TOKEN` |

## 集成步骤

### 1. 快速开始 — 原生沙箱模式（推荐）

这是最简单的集成路径。CodeBuddy CLI 内置 `--sandbox` 支持，使用 E2B 协议。由于 CubeSandbox 兼容 E2B，你可以直接将 CodeBuddy 连接到 CubeSandbox，**无需构建自定义镜像或使用 Python SDK**。

**步骤 1：设置环境变量**

```bash
export E2B_API_URL=http://<cube-host>:3000
export E2B_API_KEY=e2b_000000
export CODEBUDDY_API_KEY=ck_xxxxxxxx
```

> 不需要 `CUBE_TEMPLATE_ID`——原生沙箱模式使用默认的 `sandbox-code` 模板。

**步骤 2：使用原生沙箱路由运行 CodeBuddy**

```bash
# CodeBuddy 在你的本地机器上运行（已通过认证）；
# 工具调用（bash、文件操作）被路由到 CubeSandbox MicroVM 中执行。
# 这样避免了沙箱内的认证问题。
codebuddy --sandbox http://<cube-host>:3000 --sandbox-new \
  -p "List files in /workspace" --output-format json -y
```

预期输出（示例）：

```json
{
  "result": "Here are the files in /workspace:\n\n- README.md\n- src/\n- package.json\n",
  "session_id": "cb_sess_abc123",
  "usage": {
    "prompt_tokens": 45,
    "completion_tokens": 32,
    "total_tokens": 77
  }
}
```

此模式下：
- CodeBuddy 本身在主机上运行
- `CODEBUDDY_API_KEY` 保留在主机上（不进入沙箱）
- 工具调用（bash、read、write、edit）在 MicroVM 内执行
- `--sandbox-new` 每次调用创建新沙箱
- `--sandbox-id <id>` 重连到已有沙箱
- `--sandbox-kill` 完成后销毁沙箱
- `--sandbox-upload-dir <dir>` 将主机目录上传到沙箱

当你不需要将 CodeBuddy 预装到镜像中时，这是推荐方式。使用默认 `sandbox-code` 模板（或任何在 49983 端口运行 envd 的模板）即可。

### 2. 完整集成路径 — 沙箱内 / HTTP API 模式

当你需要 CodeBuddy **安装在沙箱内部**时（用于沙箱内 CI/CD 或 HTTP API 服务），请使用此路径。

#### 步骤 1 — 构建模板镜像

模板镜像在官方 `sandbox-code` 基础镜像（已包含 envd，即 CubeSandbox 沙箱代理）之上叠加 Node.js 22 LTS 和 CodeBuddy CLI：

```dockerfile
FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get update && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

RUN npm install -g @tencent-ai/codebuddy-code@latest

RUN mkdir -p /workspace
WORKDIR /workspace
```

#### 步骤 2 — 注册模板

构建并推送镜像，然后注册为 CubeSandbox 模板：

```bash
docker build -t <registry>/codebuddy-sandbox:latest .
docker push <registry>/codebuddy-sandbox:latest

cubemastercli tpl create-from-image \
  --image <registry>/codebuddy-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --cpu 4000 --memory 4096 \
  --probe 49983
```

记下输出中的 `template_id`。

- **端口 49983**：envd（CubeSandbox 沙箱代理，必需）
- **端口 8080**：CodeBuddy HTTP API（`codebuddy --serve`，可选——仅 HTTP API 模式需要）

#### 步骤 3 — 配置环境变量

```bash
export E2B_API_URL=http://<cube-host>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<template-id>
export CODEBUDDY_API_KEY=ck_xxxxxxxx
```

> **认证限制**：沙箱内模式和 HTTP API 模式需要 `CODEBUDDY_AUTH_TOKEN`（仅限企业/CI 账户）。沙箱基础设施（创建、文件上传、脚本执行）已验证正常，但缺少该凭证，codebuddy 在沙箱内的实际对话未能完成验证。个人账户应使用原生沙箱模式，只需 `CODEBUDDY_API_KEY` 即可。

#### 步骤 4 — 在沙箱内运行 CodeBuddy

沙箱内模式（最简方式）：

```python
from e2b_code_interpreter import Sandbox

sb = Sandbox.create(template="your-template-id")
result = sb.commands.run(
    'codebuddy -p "List files in /workspace" --output-format json --max-turns 10',
    user="root"
)
print(result.stdout)
sb.kill()
```

预期输出（示例）：

```json
{
  "result": "The /workspace directory contains: README.md, src/, package.json",
  "session_id": "cb_sess_def456",
  "usage": {
    "prompt_tokens": 45,
    "completion_tokens": 28,
    "total_tokens": 73
  }
}
```

## 关键代码示例

### 1. 沙箱内执行模式

最简集成方式——运行单次 CodeBuddy 调用：

```python
from e2b_code_interpreter import Sandbox
import json
import os

sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

cmd = (
    f'codebuddy -p "Analyze the code in /workspace" '
    f'--output-format json --max-turns 10 '
    f'-y'
)
result = sb.commands.run(cmd, user="root")

output = json.loads(result.stdout)
print(output["result"])
print(f"Session: {output['session_id']}")
print(f"Tokens: {output.get('usage', {})}")

sb.kill()
```

### 2. HTTP API 模式

适用于交互式或长时间运行场景：

```python
from e2b_code_interpreter import Sandbox
import time
import os

sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

# 在后台启动 codebuddy --serve
sb.commands.run(
    'nohup codebuddy --serve --port 8080 --hostname 0.0.0.0 '
    '> /tmp/codebuddy.log 2>&1 &',
    user="root"
)

# 等待服务器就绪
for _ in range(30):
    time.sleep(1)
    health = sb.commands.run('curl -s http://localhost:8080/health', user="root")
    if "ok" in health.stdout:
        break

# 调用聊天 API
result = sb.commands.run(
    'curl -s -X POST http://localhost:8080/api/chat '
    '-H "Content-Type: application/json" '
    '-d \'{"message": "Hello!"}\'',
    user="root"
)
print(result.stdout)
sb.kill()
```

## 注意事项

- **envd 用户**：CubeSandbox 的 envd 仅服务于 `root` 用户。在 `sb.commands.run()` 和 `sb.files.*` 调用中务必传入 `user="root"`。
- **API 密钥注入**：`CODEBUDDY_API_KEY` 必须在沙箱内可用。可直接写入镜像（生产环境不推荐），或通过 CubeEgress 凭据保险库注入（推荐——密钥不会进入沙箱文件系统）。
- **网络出口**：CodeBuddy 需要出站 HTTPS 访问 LLM API 端点。在 CubeEgress 出口策略中为 `api.codebuddy.ai`（或自定义 LLM 端点）配置允许列表。如需更严格控制，使用 `Sandbox.create(allow_internet_access=False)` + CIDR 允许列表。
- **SSL 证书**：如使用 CubeSandbox 自签名证书，请将 `CUBE_SSL_CERT_FILE` 设为 CA 证书路径。CodeBuddy 的 Node.js 运行时使用系统 CA 存储。
- **资源限制**：CodeBuddy 处理大型代码库时可能需要 >2GB 内存。创建模板时设置 `--memory 4096`（或更高）。
- **最大轮次**：在 CLI 上务必设置 `--max-turns` 以防止沙箱内模式下无限工具调用循环。
- **模型可用性**：CodeBuddy 中国版（`codebuddy.cn`，含 GLM/MiniMax/Kimi/混元模型）与国际版（`codebuddy.ai`，含 GPT/Claude/Gemini 模型）模型目录不同。`hy3-preview` 在 `codebuddy.cn` 中可用。可通过 `--model` 覆盖。
- **镜像仓库**：国际访问使用 `cube-sandbox-int.tencentcloudcr.com`，中国大陆使用 `cube-sandbox-cn.tencentcloudcr.com`。
- **沙箱内认证**：沙箱内模式和 HTTP API 模式需要 `CODEBUDDY_AUTH_TOKEN` 才能在沙箱内完成认证，该 token 当前仅企业/CI 账号开放。沙箱基础设施（创建、文件上传、脚本执行）已验证正常，codebuddy 在沙箱内的实际对话因缺少该凭证未能完成验证。

## 参考资料

- 相关文档：
  - [CubeSandbox 快速开始](../quickstart.md)
  - [CubeSandbox 模板](../templates.md)
  - [CubeSandbox 快照与回滚](../snapshot-rollback-clone.md)
  - [CubeSandbox 网络策略](../network-policy.md)
- 示例仓库：[`examples/codebuddy-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
- 上游项目：
  - [CodeBuddy CLI 文档](https://www.codebuddy.ai/docs/cli/overview)
  - [CodeBuddy API 密钥](https://www.codebuddy.ai/profile/keys)
  - [CubeSandbox GitHub 仓库](https://github.com/TencentCloud/CubeSandbox)
