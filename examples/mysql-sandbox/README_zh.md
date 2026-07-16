# MySQL 沙箱示例

[English](README.md)

Cube Sandbox 的轻量级 MySQL 客户端沙箱，提供隔离的 SQL 执行环境，支持网络策略控制和快照功能。

## 概述

本示例演示如何构建 MySQL 客户端沙箱，实现以下功能：
- 从隔离的 KVM 虚拟机连接 MySQL 数据库
- 强制执行网络隔离策略（白名单/黑名单）
- 创建快照以保存和回滚状态

### 核心特性

| 特性 | 说明 |
|------|------|
| **隔离执行** | 每个沙箱运行在独立的 KVM MicroVM 中，与宿主机完全隔离 |
| **网络控制** | 支持完全断网、CIDR 白名单、黑名单等精细化网络策略 |
| **快照回滚** | 支持创建快照，可恢复到任意历史状态 |
| **资源灵活** | 支持自定义 CPU、内存和存储大小 |

## 适用场景

### 1. 数据库操作

无需在本地安装 MySQL 客户端，直接通过 SDK 在沙箱中执行 SQL 查询。

**优势**：
- 无需配置本地数据库环境
- 每次使用都是全新、干净的环境
- 不用担心污染本地开发环境

### 2. AI Agent 集成

为 AI Agent 提供安全的 SQL 执行能力，可控制网络访问范围。

**典型应用**：
- 数据分析 Agent：允许访问数据分析服务器，但禁止访问外部网络
- 代码审查 Agent：只允许访问内网代码仓库数据库
- 报表生成 Agent：只允许读取数据，禁止修改

### 3. 数据库迁移测试

在隔离环境中安全测试数据库迁移，迁移失败可快速回滚。

**工作流程**：
1. 创建基线快照
2. 执行迁移脚本
3. 验证数据完整性
4. 失败时回滚到基线

### 4. 数据分析

从沙箱环境查询数据库，降低数据泄露风险。

**安全特性**：
- 沙箱销毁后数据完全清除
- 可限制只能访问特定数据库
- 支持审计日志

## 架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                        Cube Sandbox                              │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                   KVM MicroVM                            │    │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐ │    │
│  │  │  envd       │  │  MySQL      │  │  快照           │ │    │
│  │  │  (端口       │  │  客户端     │  │  (内存 +        │ │    │
│  │  │  49983)      │  │             │  │   rootfs)       │ │    │
│  │  └─────────────┘  └─────────────┘  └─────────────────┘ │    │
│  └─────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
                      ┌───────────────┐
                      │   MySQL       │
                      │   数据库      │
                      └───────────────┘
```

### 组件说明

| 组件 | 描述 |
|------|------|
| **KVM MicroVM** | 基于 KVM 的轻量级虚拟机，提供硬件级隔离 |
| **envd** | 环境守护进程，负责沙箱生命周期管理和代码执行 |
| **MySQL Client** | 预装的 MySQL 命令行客户端 |
| **快照系统** | 支持内存快照和文件系统快照 |

## 前置条件

### 必需条件

- 已部署 Cube Sandbox 环境（参见[快速开始](../../docs/guide/quickstart.md)）
- Python 3.8+
- Docker（用于构建自定义镜像）

### 环境验证

```bash
# 验证 Python 版本
python3 --version
# 应输出 Python 3.8+

# 验证 Docker
docker --version
# 应输出 Docker 版本

# 验证 Cube 环境
cubemastercli --version
# 应输出 cubemastercli 版本
```

## 使用前必读

> **重要安全警告 — 使用 `multi_query.py` 前请务必阅读！**

本沙箱会在你的 MySQL 服务器上执行 SQL。请先了解以下安全措施：

### 凭据安全

`env_utils.py` 中的 `build_mysql_cmd()` 故意省略 `-p<password>`。
`mysql` 客户端从沙箱环境变量读取 `MYSQL_PWD`，因此密码不会出现在沙箱内的
`/proc/<pid>/cmdline` 或 `ps aux` 中。

**请勿**把 `DB_PASSWORD` 注入 shell 命令行，改为通过
`Sandbox.create(envs=...)` 以 `MYSQL_PWD` 形式传入。

### `multi_query.py` 的破坏性清理

该脚本末尾会执行 `DROP DATABASE`。为防止误删数据，**DROP 需要两个独立的开关同时打开**：

1. 环境变量 `MYSQL_SANDBOX_ALLOW_DROP` 必须为真值（`1` / `true` / `yes` / `on`）。
2. 实际操作的库名必须以 `cube_demo_` 前缀开头（脚本会自动加上此前缀，
   `DB_NAME=smoke` 会被改写为 `cube_demo_smoke`）。

两者缺一则跳过 DROP，演示库不会被删除。

**在生产服务器上运行前，请将 `DB_NAME` 设置为一次性值（如 `cube_demo_$(date +%s)`）。**

## 快速开始

### 第一步：构建 Docker 镜像

本示例基于 `cubesandbox-base` 构建，包含 MySQL 客户端和常用工具。

```bash
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox
```

**预期输出**：
```
[+] Building 10.5s (8/8) FINISHED
...
 => naming to docker.io/library/cubesandbox-mysql-sandbox:latest
```

### 第二步：选择镜像仓库并推送

根据你的网络环境，选择合适的镜像仓库：

#### 选项 A：腾讯云内网（推荐国内部署）

如果 Cube 节点可以访问腾讯云内网镜像仓库：

```bash
# 标记镜像
docker tag cubesandbox-mysql-sandbox:latest \
    cube-sandbox-image.tencentcloudcr.com/your-namespace/cubesandbox-mysql-sandbox:latest

# 推送（无需认证）
docker push cube-sandbox-image.tencentcloudcr.com/your-namespace/cubesandbox-mysql-sandbox:latest
```

> **注意**：腾讯云内网镜像仓库可能需要提前申请权限。

#### 选项 B：GitHub Container Registry（ghcr.io）

如果可以访问 ghcr.io：

```bash
# 登录 GitHub Container Registry
# 需要 GitHub Personal Access Token (GITHUB_TOKEN)
echo $GITHUB_TOKEN | docker login ghcr.io -u <your-github-username> --password-stdin

# 标记并推送
docker tag cubesandbox-mysql-sandbox:latest \
    ghcr.io/<owner>/cubesandbox-mysql-sandbox:latest
docker push ghcr.io/<owner>/cubesandbox-mysql-sandbox:latest
```

#### 选项 C：私有镜像仓库

使用你自己的私有仓库：

```bash
# 登录镜像仓库
docker login <your-registry>

# 推送镜像
docker tag cubesandbox-mysql-sandbox:latest <your-registry>/cubesandbox-mysql-sandbox:latest
docker push <your-registry>/cubesandbox-mysql-sandbox:latest
```

> **网络说明**：如果 Cube 节点无法访问 Docker Hub (`registry-1.docker.io`)，请使用上述内网镜像仓库或其他可访问的仓库。

### 第三步：注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
    --image cubesandbox-mysql-sandbox:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

**参数说明**：

| 参数 | 说明 | 推荐值 |
|------|------|--------|
| `--image` | Docker 镜像地址 | 你的镜像地址 |
| `--writable-layer-size` | 可写层大小 | 1G ~ 5G |
| `--expose-port` | 暴露端口 | 49983 (envd) |
| `--probe` | 健康检查端口 | 49983 |
| `--probe-path` | 健康检查路径 | /health |

**预期输出**：
```
Template created successfully!
Template ID: tpl-xxxxxxxxxxxxxxxx
Status: PENDING
...
Status: READY
```

> **注意**：首次创建需要拉取基础镜像和构建快照，可能需要 1-3 分钟。Status 变为 READY 后方可使用。

记录输出的 `template_id`，后续会用到。

### 第四步：配置环境变量

```bash
cd /root/CubeSandbox/examples/mysql-sandbox
cp .env.example .env
```

编辑 `.env` 文件：

```bash
# 必需：Cube API 服务器地址
E2B_API_URL="http://<节点IP>:3000"

# 必需：任意非空值即可通过 SDK 校验
E2B_API_KEY="e2b_000000"

# 必需：模板 ID（从第三步获取）
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxx"

# 可选：仅当使用 Cube 内置 mkcert 证书时需要
SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"

# 可选：MySQL 数据库连接配置
DB_HOST="localhost"
DB_USER="root"
DB_PASSWORD=""
DB_NAME="test_db"
```

或直接导出环境变量：

```bash
E2B_API_KEY="e2b_000000"
E2B_API_URL="http://127.0.0.1:3000"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxx"
SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"
```

### 第五步：安装依赖

```bash
pip3 install -r requirements.txt
```

**依赖说明**：

| 依赖 | 版本 | 说明 |
|------|------|------|
| e2b-code-interpreter | >= 2.4.1 | Cube Sandbox Python SDK |
| python-dotenv | 最新 | 环境变量加载 |

### 第六步：运行示例

详见下方 [示例脚本](#示例脚本) 章节。

## 示例脚本

每个脚本可独立运行，详细代码请直接查看对应文件。

| 脚本 | 用途 | 前置条件 |
|------|------|----------|
| `check_mysql.py` | 验证 MySQL 客户端在沙箱中可用 | 仅需模板 |
| `run_query.py` | 连接 MySQL 服务器并执行查询 | 需要可访问的 MySQL 服务 |
| `multi_query.py` | 批量执行多条 SQL | 需要可访问的 MySQL 服务 |
| `network_isolated.py` | 演示三种网络隔离策略 | 仅需模板 |
| `snapshot_demo.py` | 演示快照与回滚 | 仅需模板 |

运行示例：

```bash
cd /root/CubeSandbox/examples/mysql-sandbox
source .env

python3 check_mysql.py        # 验证 MySQL 客户端
python3 network_isolated.py   # 网络隔离（无需 MySQL 服务器）
python3 snapshot_demo.py      # 快照与回滚（无需 MySQL 服务器）
python3 run_query.py          # 执行 SQL（需要 DB_HOST 可达）
python3 multi_query.py        # 批量执行 SQL（需要 DB_HOST 可达）
```

预期输出样例（`check_mysql.py`）：

```
============================================================
MySQL Client Sandbox - Verification Test
============================================================
Template ID: tpl-xxxxxxxxxxxxxxxx
============================================================

[1] Creating sandbox...
[2] Sandbox created successfully!
[3] Checking MySQL client...
    MySQL version: mysql  Ver 15.1 Distrib 10.11.18-MariaDB, ...
[4] Checking available database tools...
    /usr/bin/mysql
    /usr/bin/mysqldump
[5] System information...
    Linux tpl-a067 6.6.69-opencloudos9.cubesandbox.pvm.guest-gb85200d80fa2 ...
[6] Verifying sandbox environment...
    PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"

Sandbox verification completed!
```

## 已知限制

- 沙箱仅提供 MySQL **客户端**，需要外部 MySQL 服务器。KVM 沙箱与宿主机网络隔离，无法访问 `localhost` / `127.0.0.1` 上的服务（`cube-egress` 默认只代理 `tcp dport 80/443`）。
- 默认 `--writable-layer-size 1G`，快照大小受此限制；写入大量临时文件会拖慢快照创建。
- 集群对沙箱并发数和快照数量有限制。

## 故障排除

| 问题 | 可能原因 | 解决方案 |
|------|----------|----------|
| `Template not found` | 模板 ID 错误或未就绪 | `cubemastercli tpl list` 检查状态 |
| `Connection refused` | MySQL 未运行或端口不通 | 检查 `DB_HOST` / 端口；确认沙箱能路由到目标 IP |
| `Can't connect to server` | 网络隔离阻止连接 | 调整 `allow_out`/`deny_out` CIDR |
| `SSL certificate error` | HTTPS 但未配置 CA 证书 | 设置 `SSL_CERT_FILE` 环境变量 |
| `create-from-image` 卡在 UNPACKING 20% | 跨境出口带宽过窄，registry 拉取缓慢/中断 | 切换到内网镜像仓库，或使用 `cubemastercli tpl commit` 方式 |

调试示例：

```python
with Sandbox.create(template=template_id) as sandbox:
    info = sandbox.get_info()
    print(f"沙箱 ID: {info.sandbox_id}, 状态: {info.status}")
    print(sandbox.commands.run("mysql --version").stdout)
```

模板构建日志：

```bash
cubemastercli tpl info --template-id <template-id>
```

## 目录结构

```
mysql-sandbox/
├── Dockerfile                 # Docker 镜像定义
├── README.md                  # 英文文档
├── README_zh.md              # 中文文档（本文件）
├── requirements.txt          # Python 依赖
├── .env.example              # 环境变量模板
│
├── env_utils.py              # 共享环境工具函数
│
├── check_mysql.py            # 验证 MySQL 客户端安装
├── run_query.py              # 执行 SQL 查询示例
├── network_isolated.py       # 网络隔离演示
├── snapshot_demo.py          # 快照和回滚演示
└── multi_query.py            # 执行多条查询
```

## 相关示例

| 示例 | 路径 | 说明 |
|------|------|------|
| 基础沙箱 | [code-sandbox-quickstart](../code-sandbox-quickstart/) | 沙箱基础操作入门 |
| 网络策略 | [network-policy](../network-policy/) | 更多网络配置示例 |
| 快照管理 | [snapshot-rollback-clone](../snapshot-rollback-clone/) | 快照完整功能 |
| AI Agent | [openai-agents-example](../openai-agents-example/) | OpenAI Agents 集成 |
| 浏览器沙箱 | [browser-sandbox](../browser-sandbox/) | Playwright 浏览器自动化 |

完整 `Sandbox.create()` / `create_snapshot()` / `list_snapshots()` 等 API 用法请参考 [e2b-code-interpreter SDK 文档](https://github.com/cube-sandbox/e2b-code-interpreter)。
