# MySQL 沙箱验证指南

本文档说明如何在提 PR 前验证 MySQL 沙箱示例的功能。

## 前提条件

1. Cube Sandbox 环境已部署
2. Docker 已安装并可用
3. Python 3.8+ 已安装

## 验证步骤

### 步骤 1: 构建 Docker 镜像

```bash
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-sandbox:latest examples/mysql-sandbox
```

**预期结果**：镜像构建成功，输出 `Successfully tagged`

### 步骤 2: 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
    --image cubesandbox-mysql-sandbox:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --probe 49983 \
    --probe-path /health
```

**预期结果**：
```
submitted template image job: template_id=tpl-xxxxxxxxxxxxxxxx
● Building template from image
  template tpl-xxxxxxxxxxxxxxxx
  ✓ PULLING
  ✓ UNPACKING
  ✓ BUILDING_EXT4
  ✓ GENERATING_JSON
  ✓ DISTRIBUTING
  ✓ CREATING_TEMPLATE
  Status: READY
```

### 步骤 3: 设置环境变量

```bash
cp examples/mysql-sandbox/.env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<节点IP>:3000
export CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxx"
```

### 步骤 4: 安装 Python 依赖

```bash
cd /root/CubeSandbox/examples/mysql-sandbox
pip3 install -r requirements.txt
```

### 步骤 5: 运行验证脚本

#### 5.1 基础环境检查

```bash
python3 check_mysql.py
```

**预期输出**：
```
============================================================
MySQL Client Sandbox - Environment Check
============================================================
Template ID: tpl-xxxxxxxxxxxxxxxx
API URL: http://<节点IP>:3000
============================================================

[1] Creating sandbox...
[2] Sandbox created: sb-xxxxxxxxxxxxxxxx

[3] Checking MySQL client version...
    MySQL version: mysql  Ver 8.0.xx ...

[4] Checking available database tools...
    /usr/bin/mysql
    /usr/bin/mysqldump

[5] Verifying sandbox environment...
    PRETTY_NAME="Debian GNU/Linux 12"
    内核: Linux ... x86_64 GNU/Linux

[6] Connection Information:
    DB_HOST: localhost (use port mapping or external host)
    DB_USER: root
    DB_PORT: 3306
    To connect to an external MySQL server, set envs:
    Sandbox.create(template=template_id, envs={'DB_HOST': 'your-host.com'})

============================================================
Sandbox verification completed!
============================================================
```

#### 5.2 快照功能演示

```bash
python3 snapshot_demo.py
```

**预期输出**：
```
============================================================
MySQL Sandbox - Snapshot Demo
============================================================
[1] Creating initial sandbox...
    Sandbox created: sb-xxxxxxxxxxxxxxxx
[2] Setting initial state...
    Initial content: v1.0 - Initial state
[3] Creating snapshot...
    Snapshot ID: snap-xxxxxxxxxxxxxxxx
[4] Modifying state...
    Modified content: v2.0 - Modified state
[5] Waiting 2 seconds...
[6] Restoring from snapshot...
    Restored content: v1.0 - Initial state

    ✓ State successfully restored to v1.0!
============================================================
Snapshot test passed!
============================================================
```

#### 5.3 网络隔离演示

```bash
python3 network_isolated.py
```

**预期输出**：
```
============================================================
Network Isolation Demo
============================================================
[1] Creating sandbox with NO internet access...
[2] Sandbox created: sb-xxxxxxxxxxxxxxxx
    Network policy: allow_internet_access=False
[3] Testing network isolation...
    Attempting to reach google.com...
    ✓ Network isolation verified: External access blocked
[4] Verifying local tools still work...
    ✓ MySQL client: mysql  Ver 8.0.xx ...
    ✓ Local commands: Local network works!
============================================================
Network isolation test completed!
============================================================
```

## 截图要求

提 PR 时，请提供以下截图：

| 截图 | 内容 |
|------|------|
| 1 | Docker 镜像构建成功 |
| 2 | 模板创建成功（Status: READY） |
| 3 | check_mysql.py 运行成功 |
| 4 | snapshot_demo.py 运行成功（可选，如环境支持） |
| 5 | network_isolated.py 运行成功 |

## 常见问题

### 问题 1: Template not found

**原因**：模板 ID 错误或模板未就绪

**解决**：
```bash
# 检查模板状态
cubemastercli tpl list

# 等待模板就绪
cubemastercli tpl watch --job-id <job-id>
```

### 问题 2: Network timeout when pulling image

**原因**：Cube 节点无法访问 Docker Hub

**解决**：

#### 方案 A：推送到可访问的镜像仓库

根据网络环境选择合适的镜像仓库：

```bash
# 腾讯云内网镜像仓库
docker tag cubesandbox-mysql-sandbox:latest \
    cube-sandbox-image.tencentcloudcr.com/your-namespace/cubesandbox-mysql-sandbox:latest
docker push cube-sandbox-image.tencentcloudcr.com/your-namespace/cubesandbox-mysql-sandbox:latest

# 或使用 GitHub Container Registry
docker tag cubesandbox-mysql-sandbox:latest \
    ghcr.io/<owner>/cubesandbox-mysql-sandbox:latest
docker push ghcr.io/<owner>/cubesandbox-mysql-sandbox:latest
```

#### 方案 B：手动加载镜像

在有 Docker Hub 访问权限的机器上：

```bash
# 在能访问 Docker Hub 的机器上
docker pull cubesandbox-mysql-sandbox:latest
docker save cubesandbox-mysql-sandbox:latest -o /tmp/mysql.tar

# 将 tar 包复制到 Cube 节点
scp /tmp/mysql.tar cube-node:/tmp/

# 在 Cube 节点上加载
docker load -i /tmp/mysql.tar
```

### 问题 3: mysql: command not found

**原因**：模板未包含 MySQL 客户端

**解决**：
1. 确保 Dockerfile 正确安装 mysql-client
2. 重新构建模板

### 问题 4: ImportError: No module named 'env_utils'

**原因**：缺少 env_utils.py 文件

**解决**：
1. 确保所有 Python 脚本都在 `examples/mysql-sandbox/` 目录下
2. 检查是否有 `env_utils.py` 文件

### 问题 5: create_snapshot() not available

**原因**：快照功能需要 CubeSandbox 0.3.0+

**解决**：
1. 检查 CubeSandbox 版本
2. 快照演示脚本会自动跳过此功能并输出提示

## 验证检查清单

- [ ] Docker 镜像构建成功
- [ ] 模板状态为 READY
- [ ] check_mysql.py 显示 MySQL 版本信息
- [ ] snapshot_demo.py 成功创建和恢复快照（如果环境支持）
- [ ] network_isolated.py 网络隔离功能正常

## 目录结构

```
mysql-sandbox/
├── Dockerfile              # Docker 镜像定义
├── README.md              # 英文文档
├── README_zh.md           # 中文文档
├── check_mysql.py         # 环境检查脚本
├── snapshot_demo.py       # 快照演示脚本
├── network_isolated.py    # 网络隔离演示脚本
├── env_utils.py           # 环境变量加载工具
├── requirements.txt       # Python 依赖
├── .env.example           # 环境变量模板
├── VERIFICATION.md        # 验证指南
└── screenshots/          # 验证截图（由贡献者添加）
    ├── 01_check_mysql.png
    ├── 02_snapshot_demo.png
    ├── 03_network_isolated.png
    └── 04_tpl_list.png
```
