# Claude Code 与 CubeSandbox 集成

[English](./README.md)

可以在 **CubeSandbox** MicroVM 中运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code)，也可以让 Claude Code 留在宿主机，将它的 Bash 命令透明转发到 MicroVM 中执行。

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
| **透明 Bash 隔离** | 可选 `PreToolUse` Hook 将宿主机 Claude Code 的 Bash 调用转发到可复用沙箱 |

## 快速开始

需要 Python 3.9 或更高版本。

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
python3 -m pip install -r requirements.txt
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
├── Dockerfile              # 构建预装 Claude Code 的沙箱镜像
├── run_claude_code.py      # 单次执行入口
├── resume_claude_code.py   # 暂停与恢复示例
├── network_policy.py       # 默认拒绝出口与凭据注入
├── env_utils.py            # 提供商和命令构造工具
├── _common.py              # 共用沙箱初始化与命令执行工具
├── mcp_server.py           # 可选 MCP 服务
├── sandbox_exec.py         # 独立沙箱执行工具
├── hooks/                  # 宿主机 Claude Code PreToolUse Hook 与安装脚本
│   ├── cubesandbox_exec.py
│   ├── cubesandbox_rewrite.py
│   └── install.sh
├── tests/                  # 自动化测试
│   ├── conftest.py
│   ├── test_common.py
│   ├── test_cubesandbox_exec.py
│   ├── test_cubesandbox_rewrite.py
│   ├── test_env_utils.py
│   ├── test_hook_install.py
│   ├── test_mcp_server.py
│   └── test_sandbox_exec.py
├── requirements.txt        # Python 依赖
├── .env.example            # 配置模板
├── .gitignore
├── TROUBLESHOOTING.md      # 详细故障排查
├── README.md               # 英文文档
└── README_zh.md            # 本文档
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

### 场景 D：宿主机 Claude Code + `PreToolUse` Hook

该方案让 Claude Code 留在宿主机，并将每个 Claude Code `Bash` 工具调用改写后通过原生 CubeSandbox SDK 执行：

```
Claude Code（宿主机）
    |-- Read / Write / Edit ----------> 宿主机项目
    |
    `-- Bash --> PreToolUse Hook --> CubeAPI --> 可复用 MicroVM
                                                `-- 只读挂载项目目录
```

完成第二步后安装 Hook，并重启 Claude Code：

```bash
cd hooks
./install.sh
```

安装脚本会在不覆盖其他配置的前提下更新 `~/.claude/settings.json`，并且只把 `../.env` 中白名单内的 `CUBE_*` 配置写入 Hook 配置，不会复制模型提供商的 API 密钥。

Hook 按 Claude Code `session_id` 复用沙箱，默认在 `~/.cache/cubesandbox-hook/` 保存会话与沙箱的映射，并在多次 Bash 调用间保留 `cd` 和导出的环境变量。

首次执行 Bash 时，项目目录可以相同路径**只读**挂载进沙箱。沙箱命令可以读取宿主机项目，但不能编辑项目文件或向其中写入构建产物。Claude Code 的 `Read`、`Write`、`Edit` 工具仍在宿主机执行；该 Hook 只覆盖 `Bash`。

`hostPath` 在调度到的 Cubelet 节点上解析，而不是在运行 Claude Code 的机器上解析。因此，只有 Claude Code 与 Cubelet 位于同一节点，或项目已通过共享存储/同步机制存在于每个候选 Cubelet 的相同绝对路径时，才能得到一致文件视图。Hook 不会把本地项目上传或同步到远端部署；不要直接放行只存在于客户端、但可能在 Cubelet 上指向其他数据的路径。

项目路径必须在 CubeMaster 的允许列表中：

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

如果挂载被拒绝，命令会降级到没有挂载的独立沙箱中执行。Bash 仍然隔离，但无法再与宿主机文件工具保持一致的文件视图。

开始无关任务前可以重置指定会话；卸载前也应先重置仍在使用的沙箱：

```bash
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

cd hooks
./install.sh --uninstall
```

## 常见问题

| 问题 | 可能原因 | 解决方法 |
|------|----------|----------|
| `claude: command not found` | 模板未安装 Claude Code | 使用 Dockerfile 构建镜像或启动时安装 |
| `ANTHROPIC_AUTH_TOKEN not set` | 缺少 `.env` 配置 | 执行 `cp .env.example .env` 并填写密钥 |
| SSL 证书错误 | CubeProxy HTTPS 缺少 CA 证书 | 设置 `SSL_CERT_FILE` 或 `NODE_EXTRA_CA_CERTS` |
| 连接 LLM API 超时 | 出口策略拦截 | 检查 `resolve_llm_host()` 返回的主机名 |
| `Template not found` | 模板 ID 错误 | 执行 `cubemastercli tpl list` 检查 |
