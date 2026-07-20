# MySQL 客户端沙箱

[English](README.md)

一个预装 MySQL 客户端的轻量级沙箱环境，用于连接外部 MySQL 数据库。非常适合测试数据库操作、运行迁移脚本和在隔离环境中执行 SQL 查询。

## 1. 背景

**Cube Sandbox** 是一个轻量级 MicroVM 平台，完全兼容 [E2B SDK](https://e2b.dev)。这个 MySQL 客户端沙箱提供：

- 预装的 MySQL 客户端工具（`mysql`、`mysqldump`）
- 网络隔离能力（可限制出站访问）
- 快照和恢复，支持有状态的数据库测试
- 硬件级隔离，运行不受信任的 SQL 脚本

```
┌──────────────────────┐         ┌─────── Cube Sandbox ──────────────┐
│                      │         │                                    │
│  你的脚本            │  MySQL  │  ┌───────────────────────────┐    │
│  (Python / Shell)   │────────►│  │  mysql 客户端              │    │
│                      │   Wire  │  │  /usr/bin/mysql           │    │
│                      │         │  └───────────────────────────┘    │
└──────────────────────┘         │                                    │
                                 │  ┌───────────────────────────┐    │
                                 │  │  外部 MySQL 服务器         │    │
                                 │  │  (任意可访问的主机)        │    │
                                 │  └───────────────────────────┘    │
                                 └────────────────────────────────────┘
```

## 2. 适用场景

- **数据库测试**：对测试数据库运行 SQL 查询和迁移
- **数据迁移**：在隔离环境中执行 `mysqldump` 操作
- **ORM 验证**：测试各种框架的数据库连接
- **安全测试**：以网络隔离方式运行不受信任的 SQL 脚本
- **CI/CD 集成**：在临时隔离环境中执行数据库操作

## 3. 前置条件

- 已部署的 Cube Sandbox 环境
- Python 3.8+

```bash
pip install -r requirements.txt
```

## 4. 快速开始

### 第一步 — 构建并创建模板

```bash
# 构建 Docker 镜像
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox

# 注册为 Cube 模板
cubemastercli tpl create-from-image \
    --image cubesandbox-mysql-sandbox:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

记录输出的 `template_id`（格式：`tpl-xxxxxxxxxxxxxxxx`）。

### 第二步 — 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

或直接导出：

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<你的节点IP>:3000
export CUBE_TEMPLATE_ID=<template-id>
```

### 第三步 — 运行示例

```bash
python check_mysql.py
```

预期输出：

```
============================================================
MySQL 客户端沙箱 - 环境检查
============================================================
模板 ID: tpl-xxxxxxxxxxxxxxxx
API URL: http://<节点IP>:3000
============================================================

[1] 创建沙箱...
[2] 沙箱创建成功: sb-xxxxxxxxxxxxxxxx

[3] 检查 MySQL 客户端版本...
    MySQL 版本: mysql  Ver 8.0.xx ...

[4] 检查可用数据库工具...
    /usr/bin/mysql
    /usr/bin/mysqldump

[5] 验证沙箱环境...
    PRETTY_NAME="Debian GNU/Linux 12"
    内核: Linux ... x86_64 GNU/Linux

============================================================
沙箱验证完成！
============================================================
```

## 5. 所有示例

| 脚本 | 演示内容 |
|------|---------|
| `check_mysql.py` | 基础 MySQL 客户端环境检查 |
| `run_query.py` | 执行 SQL 查询 |
| `multi_query.py` | 多步骤数据库操作（DDL + DML） |
| `snapshot_demo.py` | 快照、修改和恢复沙箱状态 |
| `network_isolated.py` | 无互联网访问的网络隔离 |

### check_mysql.py — 环境检查

验证 MySQL 客户端和工具是否正确安装：

```python
with Sandbox.create(template=template_id) as sandbox:
    # 检查 MySQL 客户端版本
    result = sandbox.commands.run("mysql --version")
    print(result.stdout)
```

### run_query.py — 执行 SQL 查询

对 MySQL 服务器执行 SQL 查询：

```bash
export DB_HOST=your-mysql-server.com
export DB_USER=testuser
export DB_PASSWORD=testpass
python run_query.py
```

### multi_query.py — 多步骤数据库操作

按顺序运行多个查询（CREATE、INSERT、SELECT）：

```bash
export DB_HOST=your-mysql-server.com
export DB_USER=testuser
export DB_PASSWORD=testpass
export DB_NAME=smoke
export MYSQL_SANDBOX_ALLOW_DROP=1
python multi_query.py
```

> **安全提示**：DROP DATABASE 操作受双重确认保护：
> 1. `MYSQL_SANDBOX_ALLOW_DROP=1` 环境变量
> 2. 数据库名必须以 `cube_demo_` 前缀开头

### snapshot_demo.py — 快照与恢复

演示 CubeSandbox 的快照功能，用于数据库测试：

```python
with Sandbox.create(template=template_id) as sandbox:
    # 创建初始快照
    snapshot_id = sandbox.create_snapshot()

    # 进行修改（例如创建表、插入数据）
    sandbox.commands.run("mysql -h $DB_HOST -u $DB_USER ...")

    # 恢复到初始状态
    sandbox.restore_snapshot(snapshot_id)
```

### network_isolated.py — 网络隔离

创建无出站互联网访问的沙箱，用于安全测试：

```python
sandbox = Sandbox.create(
    template=template_id,
    allow_internet_access=False  # 无出站网络
)
```

## 6. 连接到 MySQL 服务器

### 在沙箱内

```bash
# 连接到 MySQL 服务器
mysql -h <主机> -P <端口> -u <用户> -p<密码>

# 运行 SQL 文件
mysql -h <主机> -u <用户> < database.sql

# 导出数据库
mysqldump -h <主机> -u <用户> --all-databases > backup.sql
```

### 环境变量

创建沙箱时设置：

```python
sandbox = Sandbox.create(
    template=template_id,
    envs={
        "DB_HOST": "your-mysql-host.com",
        "DB_PORT": "3306",
        "DB_USER": "testuser",
        "DB_PASSWORD": "testpass",
        "DB_NAME": "testdb"
    }
)
```

## 7. 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| `mysql: command not found` | 模板构建不正确 | 重新构建 Docker 镜像 |
| `Connection refused` | MySQL 服务器不可达 | 检查 DB_HOST 和网络策略 |
| `Access denied` | 凭据错误 | 验证 DB_USER 和 DB_PASSWORD |
| `Can't connect to MySQL server` | 防火墙或网络策略 | 检查 allow_out 规则 |

## 8. 目录结构

```
mysql-sandbox/
├── Dockerfile              # Docker 镜像定义
├── README.md              # 英文文档
├── README_zh.md           # 中文文档（本文档）
├── check_mysql.py         # 环境检查脚本
├── run_query.py           # 执行 SQL 查询
├── multi_query.py         # 多步骤数据库操作
├── snapshot_demo.py       # 快照与恢复演示
├── network_isolated.py    # 网络隔离演示
├── env_utils.py           # 环境变量工具
├── requirements.txt       # Python 依赖
├── ruff.toml             # 代码检查配置
├── .env.example           # 环境变量模板
├── VERIFICATION.md        # 贡献者验证指南
└── screenshots/          # 验证截图
    ├── 01_check_mysql.png
    ├── 02_network_isolated.png
    ├── 03_snapshot_demo.png
    └── 04_tpl_list.png
```
