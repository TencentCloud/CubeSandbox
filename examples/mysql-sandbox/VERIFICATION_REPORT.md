# MySQL 沙箱验证报告

> 本文档是 mysql-sandbox 示例的验证记录。

## 验证环境

| 项 | 值 |
|----|----|
| 节点 IP | `<node-ip>` |
| CubeMaster 版本 | `v0.5.1` |
| cubelet 版本 | `v0.5.1` |
| Cubelet 通信端口 | `9999` |
| CubeMaster API 端口 | `8089` |
| Cube-API 端口 | `3000` |
| 工作模板 ID | `<template-id>` |
| 基础快照 | `<snapshot-id>` |
| OS | `<os-version>` |

## 网络拓扑发现

> **注意**：以下内容为测试期间的网络环境说明，可能因节点而异。

| Registry | 状态 |
|----------|------|
| `ghcr.io` (GitHub) | 跨境带宽有限，需公网通畅 |
| `quay.io` | 可能受网络限制 |
| `registry-1.docker.io` (Docker Hub) | 依赖网络可达性 |
| `docker.m.daocloud.io` | 可能需认证 |

## 镜像构建

```bash
cd /root/CubeSandbox
docker build -t cubesandbox-mysql-client:latest examples/mysql-sandbox
```

镜像大小约 226 MB（压缩约 55.7 MB）。

## 推送镜像（示例）

```bash
docker login ghcr.io -u <your-github-username>
docker push ghcr.io/<your-github-username>/cubesandbox-mysql-client:latest
```

> **注意**：镜像推送需要跨境网络通畅。

## 模板创建

### 方案 A：`create-from-image`

```bash
cubemastercli tpl create-from-image \
    --image ghcr.io/<your-username>/cubesandbox-mysql-client:latest \
    --writable-layer-size 1G \
    --expose-port <port> \
    --probe <port> \
    --probe-path /health
```

### 方案 B：`commit sandbox`

通过在沙箱内安装依赖后提交为模板：

1. 创建基础沙箱
2. 在沙箱内安装 `mysql-client` 及相关工具
3. 使用 `cubemastercli tpl commit` 提交

## 模板验证

### 环境变量配置

```
E2B_API_URL=http://<node-ip>:3000
E2B_API_KEY=<your-api-key>
CUBE_TEMPLATE_ID=<template-id>
SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

## 示例脚本测试

| 脚本 | 说明 |
|------|------|
| `check_mysql.py` | 验证 MySQL 客户端可用性 |
| `snapshot_demo.py` | 快照创建与恢复 |
| `network_isolated.py` | 网络隔离策略测试 |
| `run_query.py` | 执行 SQL 查询 |
| `multi_query.py` | 批量执行 SQL |

## 网络隔离策略

| 场景 | 配置 |
|------|------|
| 完全隔离 | `allow_internet_access=False` |
| 仅允许私网 | `network={"allow_out": ["10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"]}` |

## 注意事项

1. **镜像拉取**：跨境环境下镜像拉取可能受限，建议使用 commit 方式创建模板
2. **数据库连接**：KVM 沙箱与主机网络天然隔离，如需连接主机数据库需配置相应网络策略
3. **安全**：生产环境请使用强密码和正确的网络策略
