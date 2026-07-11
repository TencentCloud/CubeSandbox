# Claude Code 与 CubeSandbox 集成

[English](./README.md)

在 **CubeSandbox** MicroVM 中运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code) 终端型 AI 编码 Agent，获得隔离、可复现、安全的开发环境。

```
宿主机
    │  Python SDK (e2b-code-interpreter)
    ▼
CubeAPI (端口 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           Claude Code CLI
                               │
                           npm / Node.js
```

## 核心特性

| 特性 | 说明 |
|------|------|
| **隔离执行** | 每个 Claude Code 会话在独立 MicroVM 中运行 |
| **E2B 兼容** | 使用标准 E2B SDK，兼容所有 E2B 客户端 |
| **快照持久化** | 暂停/恢复 Claude Code 会话，完整保留上下文 |
| **安全密钥注入** | CubeEgress 在线路上注入 API 密钥，沙箱内无真实凭证 |
| **网络策略** | 默认拒绝出口流量，仅放行 LLM API 地址 |

## 快速开始

### 第一步 — 构建模板

构建预装 Claude Code 的沙箱镜像：

```bash
docker build -t claude-code-sandbox:v1 .
cubemastercli tpl create-from-image \
  --image claude-code-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

### 第二步 — 配置环境

```bash
cp .env.example .env
# 编辑 .env 填入 API 密钥和模板 ID
```

### 第三步 — 运行 Claude Code

```bash
# 单次执行
python run_claude_code.py "写一个打印斐波那契数列的 Python 函数"

# 安全网络策略（推荐生产环境使用）
python network_policy.py "用一段话解释什么是 Unix pipe"

# 暂停/恢复长时间任务
python resume_claude_code.py "创建一个简单的 Python Web 服务"
```

## 目录结构

```
claude-code-integration/
├── run_claude_code.py      # 单次执行入口
├── resume_claude_code.py   # 暂停与恢复示例
├── network_policy.py       # 默认拒绝出口与凭据注入
├── env_utils.py            # 提供商和命令构造工具
├── _common.py              # 共用沙箱初始化与命令执行工具
├── mcp_server.py           # 可选 MCP 服务
├── sandbox_exec.py         # 独立沙箱执行工具
├── tests/                  # 自动化测试
├── requirements.txt
└── .env.example
```

## 使用场景

### 场景 A：隔离代码开发

Claude Code 在沙箱内编辑文件、运行命令，不影响宿主机环境。适合：
- 安全审查不确定的代码
- 在隔离环境中测试 AI 生成的代码
- 为每个项目创建独立的开发环境

### 场景 B：断点续跑

利用 CubeSandbox 快照能力，将长时间运行的 Claude Code 任务暂停保存，需要时恢复继续。

### 场景 C：代码执行与结果回收

让编排系统按需拉起沙箱执行 Claude Code 生成的代码，收集结果后销毁沙箱。

## 常见问题

| 问题 | 可能原因 | 解决方法 |
|------|----------|----------|
| `claude: command not found` | 模板未安装 Claude Code | 使用 Dockerfile 构建镜像或启动时安装 |
| `ANTHROPIC_AUTH_TOKEN not set` | 缺少 `.env` 配置 | 执行 `cp .env.example .env` 并填写密钥 |
| SSL 证书错误 | CubeProxy HTTPS 缺少 CA 证书 | 设置 `SSL_CERT_FILE` 或 `NODE_EXTRA_CA_CERTS` |
| 连接 LLM API 超时 | 出口策略拦截 | 检查 `resolve_llm_host()` 返回的主机名 |
| `Template not found` | 模板 ID 错误 | 执行 `cubemastercli tpl list` 检查 |
