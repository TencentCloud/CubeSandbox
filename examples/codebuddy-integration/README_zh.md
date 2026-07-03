# CodeBuddy CLI × CubeSandbox 示例

[English](README.md)

本目录提供了在 CubeSandbox MicroVM 中运行 [CodeBuddy CLI](https://www.codebuddy.ai/docs/cli/overview)（腾讯云 AI 编程助手）的集成示例。CodeBuddy 安装在 sandbox-code 基础镜像之上，通过 `e2b-code-interpreter` Python SDK 创建沙箱，支持三种运行模式：原生沙箱模式、沙箱内模式和 HTTP API 模式。

```
用户 / CI 流水线
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (端口 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API (出站)
```

## 前置条件

- **Python 3.10+** — `e2b-code-interpreter` SDK 需要 Python 3.10 或更高版本
- **CubeSandbox 平台** — 已部署且 CubeAPI 可访问（参见 [CubeSandbox 快速入门](https://github.com/TencentCloud/CubeSandbox)）
- **CodeBuddy API 密钥** — 在 [https://www.codebuddy.ai/profile/keys](https://www.codebuddy.ai/profile/keys) 生成
- **Docker** — 用于构建沙箱镜像和注册模板

## 快速开始

### 1. 构建 Docker 镜像并注册模板（仅沙箱内 / HTTP API 模式需要）

```bash
./build_template.sh --registry <你的镜像仓库地址>
```

该脚本会：
1. 在官方 `sandbox-code` 基础镜像之上构建包含 Node.js 22 + CodeBuddy CLI 的 Docker 镜像
2. 推送至你的容器镜像仓库
3. 通过 `cubemastercli tpl create-from-image` 注册为 CubeSandbox 模板

请记录输出的 **模板 ID**，后续需要在 `.env` 文件中使用。

> **原生沙箱模式**可以跳过此步骤——无需自定义构建镜像，使用默认 `sandbox-code` 模板即可。

### 2. 安装 Python 依赖

```bash
pip install -r requirements.txt
```

### 3. 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env` 文件，填入实际值：

| 变量 | 说明 |
|------|------|
| `CODEBUDDY_API_KEY` | CodeBuddy CLI API 密钥 |
| `E2B_API_URL` | CubeAPI 地址，如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI 鉴权密钥（任意非空字符串） |
| `CUBE_TEMPLATE_ID` | 步骤 1 中创建的沙箱模板 ID（原生沙箱模式无需设置——使用默认模板） |
| `CUBE_SSL_CERT_FILE` | （可选）Cube CA 证书路径（HTTPS 时使用） |

### 4. 运行演示

```bash
# 原生沙箱模式（推荐——无需构建镜像，无需 auth_token）
python demo.py --native-sandbox
python demo.py --native-sandbox --prompt "What files are in /workspace?"

# 沙箱内模式（在 MicroVM 中运行，需 CODEBUDDY_AUTH_TOKEN，仅企业/CI 账号可用）
python demo.py
python demo.py --prompt "What files are in /workspace?"

# HTTP API 模式
python demo.py --http-api
```

## 演示模式

| 模式 | 命令 | 说明 |
|------|------|------|
| 原生沙箱 | `python demo.py --native-sandbox` | CodeBuddy 在**你的本地机器**上运行（已通过认证）；工具调用路由到沙箱。这避免了沙箱内的认证问题。 |
| 沙箱内 | `python demo.py` | 在 MicroVM 中运行 `codebuddy -p`，解析 JSON 输出 |
| HTTP API | `python demo.py --http-api` | 启动 `codebuddy --serve`，调用 REST 聊天 API（同沙箱内模式认证要求） |

### 模式对比

| 模式 | 推荐度 | 已验证 | 需要 AUTH_TOKEN？ |
|------|--------|--------|-------------------|
| 原生沙箱 | ✅ 推荐 | ✅ 已验证 | 不需要——只需 `CODEBUDDY_API_KEY` |
| 沙箱内 | ⚠️ 企业/CI 专用 | 基础设施已验证；对话未验证 | 需要 `CODEBUDDY_AUTH_TOKEN` |

### 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--prompt` | `List the files in /workspace and describe what you see.` | 发送给 CodeBuddy 的提示（沙箱内模式） |
| `--template` | `CUBE_TEMPLATE_ID` | 沙箱模板 ID |
| `--timeout` | `300` | 沙箱超时（秒） |
| `--max-turns` | `3` | CodeBuddy 最大工具调用轮数 |
| `--http-api` | — | 运行 HTTP API 模式 |
| `--native-sandbox` | — | 运行原生沙箱模式（CodeBuddy 在主机运行，工具调用在 MicroVM 中执行） |

### 核心代码

本示例使用的关键 SDK 调用：

```python
from e2b_code_interpreter import Sandbox
import os

# 从 CodeBuddy 模板创建沙箱
sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

# 沙箱内模式运行 CodeBuddy
result = sb.commands.run('codebuddy -p "list files" --output-format json', user="root")
print(result.stdout)

# 以 HTTP API 模式启动 CodeBuddy
sb.commands.run('nohup codebuddy --serve --port 8080 > /tmp/codebuddy.log 2>&1 &', user="root")

# 文件操作
sb.files.write("/workspace/note.txt", "hello", user="root")
content = sb.files.read("/workspace/note.txt", user="root")

# 清理
sb.kill()
```

## 模板构建

[`Dockerfile`](template/Dockerfile) 在官方 CubeSandbox `sandbox-code` 基础镜像之上叠加了 Node.js 22 和 CodeBuddy CLI：

```dockerfile
FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# Install Node.js 22 LTS via NodeSource
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get update && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

# Install CodeBuddy CLI globally
RUN npm install -g @tencent-ai/codebuddy-code@latest

# Create workspace directory
RUN mkdir -p /workspace
WORKDIR /workspace
```

[`build_template.sh`](build_template.sh) 脚本自动完成构建、推送和注册：

```bash
./build_template.sh --registry <你的镜像仓库地址> [--image-name codebuddy-sandbox] [--tag latest]
```

模板暴露两个端口：
- **49983** — envd 代理（所有 CubeSandbox 模板必需）
- **8080** — CodeBuddy HTTP API 服务

### 沙箱内模式（⚠️ 需企业/CI 账号）

CodeBuddy 在沙箱内部运行。SDK 集成、文件上传、脚本执行和
codebuddy 版本检查已验证正常。codebuddy 在沙箱内的实际对话
运行需 CODEBUDDY_AUTH_TOKEN（当前仅企业/CI 账号开放），因缺少
该凭证，对话环节未能完成验证。

## 故障排查

| 问题 | 可能原因 | 解决方法 |
|------|---------|----------|
| `codebuddy: command not found` | 镜像构建失败或 Node.js 未安装 | 重新构建镜像，检查 `docker build` 输出 |
| `CODEBUDDY_API_KEY not set` | 缺少环境变量 | 在 `.env` 中设置 `CODEBUDDY_API_KEY` 或通过沙箱环境变量传入 |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 缺少 CA 证书 | 设置 `CUBE_SSL_CERT_FILE` 指向 Cube CA 证书路径 |
| `Template not found` | `CUBE_TEMPLATE_ID` 错误 | 运行 `cubemastercli tpl list` 确认模板 ID |
| HTTP API 未就绪 | 服务启动较慢 | 查看沙箱内的 `/tmp/codebuddy.log` 日志 |
| LLM API 超时 | 网络出站被阻断 | 配置 CubeEgress 白名单，放行 CodeBuddy API 域名 |

## 目录结构

```
codebuddy-integration/
├── README.md              # 英文文档
├── README_zh.md           # 中文文档（本文件）
├── build_template.sh      # 构建镜像并注册为 CubeSandbox 模板
├── demo.py                # 集成演示（原生沙箱 + 沙箱内 + HTTP API）
├── .env.example           # 环境变量模板
├── requirements.txt       # Python 依赖
└── template/
    └── Dockerfile         # 镜像：sandbox-code + Node.js + codebuddy-code
```

## 相关文档

- [CodeBuddy CLI × CubeSandbox 集成指南](../../docs/zh/guide/integrations/codebuddy.md)
- [CodeBuddy CLI 文档](https://www.codebuddy.ai/docs/cli/overview)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
