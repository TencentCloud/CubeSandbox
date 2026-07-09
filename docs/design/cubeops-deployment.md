# CubeOps 部署方案

> 适用于 one-click（All-in-One / 控制节点 + 计算节点）和腾讯云集群（Terraform/TKE）两种交付形态。
> 本文档聚焦 CubeOps 服务本身的部署集成，不重复已有组件的安装细节。

## 1. 背景与目标

CubeOps 是从 CubeAPI（Rust/Axum）剥离出的独立运维后台（Go），承担原 CubeAPI 的 admin/ops API 面（AgentHub、Cluster、Store、Config、Auth）。剥离后职责划分如下：

| 服务 | 语言 | 职责 | 网络暴露 | 认证 | 状态 |
|------|------|------|----------|------|------|
| **CubeAPI** | Rust/Axum | E2B 兼容 SDK API（`/sandboxes`、`/templates`、`/snapshots`） | 公网 `:3000` | `CUBE_API_KEY` 或 `AUTH_CALLBACK_URL` | 无状态 |
| **CubeOps** | Go | 运维 API（JWT 认证、AgentHub 管理、集群监控、镜像元数据、SDK 直连 CubeMaster） | 内网 `127.0.0.1:3010` | JWT Bearer | 有状态（MySQL） |

### 部署目标

1. **零侵入集成**：复用现有 one-click 的 systemd + Docker 混合管理模型，不引入新编排系统。
2. **内网安全**：CubeOps 仅绑定 `127.0.0.1`，通过 WebUI nginx 反代对外，不直接暴露端口。
3. **共享数据库**：与 CubeMaster 共用同一 MySQL 实例（`cube_mvp`），通过 `cubedb` 共享迁移模块。
4. **向后兼容**：升级时不破坏现有 All-in-One 节点的工作流，WebUI 前端可平滑切换 API 路径。

## 2. 架构拓扑

### 2.1 All-in-One 模式（one-click 默认）

```
                         ┌─────────────────────────────────────────────┐
  浏览器 ──HTTP:12088──▶ │  WebUI (nginx container)                    │
                         │   /              → 静态文件 (web/dist)       │
                         │   /opsapi/       → CubeOps :3010  (运维)     │
                         │   /sandboxes等   → CubeOps :3010  (SDK)      │
                         │   /sandbox/      → CubeProxy (沙箱流量)      │
                         └──────┬──────────────────────┬────────────────┘
                                │                      │
                     ┌──────────▼──────────┐  ┌────────▼──────────────┐
                     │ CubeOps             │  │ CubeProxy             │
                     │ :3010 (Go)          │  │ :80/:443              │
                     │ JWT 运维 + SDK 直连  │  │ 路由+TLS              │
                     │ CubeMaster          │  └───────────────────────┘
                     └──────┬──────────────┘
                            │
                     ┌──────▼──────────────┐    ┌──────────────────────┐
                     │ CubeMaster :8089    │    │ CubeAPI :3000 (Rust) │
                     │ (gRPC + HTTP 调度)   │    │ E2B SDK API (独立)   │
                     └──────┬────────┬─────┘    │ 面向外部 SDK 客户端   │
                            │        │          └──────────┬───────────┘
                   ┌────────▼───┐ ┌──▼────────┐            │
                   │ MySQL :3306│ │Redis:6379 │            │
                   │ (cube_mvp) │ │(生命周期)  │            │
                   │ CM+Ops共享 │ └───────────┘            │
                   └────────────┘                          │
                      CubeOps 直连 CubeMaster              CubeAPI 也直连 CubeMaster
                      （不经过 CubeAPI）                    （不经 nginx，独立认证）
```

### 2.2 控制节点 + 计算节点分离模式

计算节点不运行 CubeOps、CubeAPI、WebUI，只运行数据平面组件（Cubelet、CubeShim、CubeEgress 等）。CubeOps 部署在控制节点，通过 `CUBE_MASTER_ADDR` 访问同机 CubeMaster。

### 2.3 腾讯云集群模式（Terraform/TKE）

CubeOps 作为 TKE Pod 部署在控制面，通过 Service ClusterIP 暴露给同 namespace 的 WebUI Pod。MySQL 使用云数据库（CDB），Redis 使用云 Redis。CubeOps 的 `DATABASE_URL` 和 `CUBE_MASTER_ADDR` 指向集群内 Service DNS。

## 3. 构建集成

### 3.1 加入 one-click 构建流程

CubeOps 是 Go 模块，编译方式与 CubeMaster/Cubelet 一致。在 `deploy/one-click/build-release-bundle-builder.sh` 的编译清单中新增：

```bash
# 在 builder 镜像内编译 CubeOps
cd /build/CubeOps
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /prebuilt/cubeops \
    ./cmd/cubeops
```

由于 CubeOps 依赖 `cubedb`（通过 `replace ../cubedb` 本地模块），构建时需确保 `go.work` workspace 或模块 replace 路径在 builder 内可见。builder 镜像已挂载整个仓库源码，因此 `../cubedb` 路径天然满足。

### 3.2 预编译产物覆盖

与现有组件一致，支持 `ONE_CLICK_CUBE_OPS_BIN` 环境变量指定预编译二进制：

```bash
# env.example 新增
# ONE_CLICK_CUBE_OPS_BIN=/abs/path/to/cubeops
```

### 3.3 发布包内容

在 `sandbox-package.tar.gz` 中新增：

```
CubeOps/bin/cubeops          # 预编译二进制
```

安装到目标机器路径：`/usr/local/services/cubetoolbox/CubeOps/bin/cubeops`

### 3.4 Docker 镜像构建（TKE 模式）

CubeOps 的 `docker/Dockerfile` 已就绪（多阶段构建：`golang:1.25-alpine` → `alpine:3.20`）。TKE 模式下通过 jumpserver 构建并推送：

```bash
docker build -t cube-ops:${TAG} -f CubeOps/docker/Dockerfile .
```

> **注意**：当前 Dockerfile 中 `COPY ../cubedb /build/../cubedb` 在 Docker build context 为 `CubeOps/` 时会越界。TKE 构建时需以仓库根为 context：
> ```bash
> docker build -t cube-ops:${TAG} -f CubeOps/docker/Dockerfile .
> ```
> 或在 one-click 本地二进制部署模式下不使用 Docker（直接用编译好的二进制 + systemd）。

## 4. 安装集成（All-in-One / one-click）

### 4.1 目录布局

```
/usr/local/services/cubetoolbox/
├── .one-click.env                    # 全局环境变量（所有服务共享）
├── CubeAPI/bin/cube-api              # CubeAPI 二进制
├── CubeOps/
│   └── bin/cubeops                   # CubeOps 二进制（新增）
├── CubeMaster/conf.yaml
├── scripts/systemd/
│   ├── cube-api-start.sh
│   ├── cubeops-start.sh              # 新增
│   └── cubeops-postcheck.sh          # 新增
└── ...
```

### 4.2 systemd 服务单元

新增 `cube-sandbox-cubeops.service`，遵循现有 service 模式：

```ini
[Unit]
Description=Cube Sandbox CubeOps (admin/ops API)
After=network-online.target cube-sandbox-cubemaster.service
Wants=network-online.target cube-sandbox-cubemaster.service
ConditionPathExists=/usr/local/services/cubetoolbox/.one-click.env
PartOf=cube-sandbox-control.target

[Service]
Type=simple
User=root
WorkingDirectory=/usr/local/services/cubetoolbox
EnvironmentFile=-/usr/local/services/cubetoolbox/.one-click.env
ExecStart=/usr/bin/bash /usr/local/services/cubetoolbox/scripts/systemd/cubeops-start.sh
ExecStartPost=/usr/bin/bash /usr/local/services/cubetoolbox/scripts/systemd/cubeops-postcheck.sh
Restart=on-failure
RestartSec=2s
TimeoutStartSec=60s
TimeoutStopSec=30s

[Install]
WantedBy=cube-sandbox-control.target
```

关键设计：
- `After` CubeMaster，确保依赖就绪（CubeOps 启动时连 MySQL + CubeMaster）
- CubeOps **不依赖 CubeAPI**——SDK 接口直接调 CubeMaster，运维接口也不经过 CubeAPI
- `PartOf=cube-sandbox-control.target`，随控制面统一启停

### 4.3 启动脚本 `cubeops-start.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

CUBE_OPS_BIN="${TOOLBOX_ROOT}/CubeOps/bin/cubeops"
CUBE_OPS_LOG_DIR="${CUBE_OPS_LOG_DIR:-/data/log/CubeOps}"

ensure_executable "${CUBE_OPS_BIN}"
mkdir -p "${CUBE_OPS_LOG_DIR}"

# 内网绑定，不暴露到公网
export CUBE_OPS_BIND="${CUBE_OPS_BIND:-127.0.0.1:3010}"
export CUBE_OPS_LOG_LEVEL="${CUBE_OPS_LOG_LEVEL:-info}"

# CubeMaster 地址（同机）
export CUBE_MASTER_ADDR="${CUBE_MASTER_ADDR:-http://127.0.0.1:8089}"

# CubeAPI 认证密钥（面向外部 SDK 客户端，E2B 格式）
export CUBE_API_KEY="${CUBE_API_KEY:-e2b_0000000000000000000000000000000000000000}"
# 如需回调认证模式，取消注释：
# export AUTH_CALLBACK_URL="${AUTH_CALLBACK_URL:-}"

# 数据库连接（与 CubeMaster 共享同一 MySQL）
if [[ -z "${DATABASE_URL:-}" ]]; then
  mysql_host="${CUBE_SANDBOX_MYSQL_HOST:-127.0.0.1}"
  mysql_port="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
  mysql_user="${CUBE_SANDBOX_MYSQL_USER:-cube}"
  mysql_password="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
  mysql_db="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
  export DATABASE_URL="mysql://${mysql_user}:${mysql_password}@${mysql_host}:${mysql_port}/${mysql_db}"
fi

# JWT_SECRET：如未显式设置，CubeOps 启动时自动生成并持久化到 DB
# （store.BootstrapJWTSecret），实现零配置部署
# 如需多实例共享，显式设置 JWT_SECRET 环境变量
# export JWT_SECRET=$(openssl rand -hex 32)

# Redis（可选，用于 JWT 黑名单；未设置时 logout 通过 DB 失效）
if [[ -n "${CUBE_SANDBOX_REDIS_HOST:-}" ]]; then
  export REDIS_URL="redis://${CUBE_SANDBOX_REDIS_PASSWORD:-}@${CUBE_SANDBOX_REDIS_HOST}:${CUBE_SANDBOX_REDIS_PORT:-6379}"
fi

exec "${CUBE_OPS_BIN}"
```

### 4.4 健康检查脚本 `cubeops-postcheck.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

CUBE_OPS_BIND="${CUBE_OPS_BIND:-127.0.0.1:3010}"
# 去掉 host 前缀，取 port
port="${CUBE_OPS_BIND##*:}"
health_url="http://127.0.0.1:${port}/health"

for i in $(seq 1 30); do
  if curl -sf --max-time 2 "${health_url}" >/dev/null 2>&1; then
    echo "cubeops healthy at ${health_url}"
    exit 0
  fi
  sleep 1
done
echo "ERROR: cubeops did not become healthy at ${health_url}" >&2
exit 1
```

### 4.5 注册到 control target

在 `cube-sandbox-control.target` 中新增：

```ini
[Unit]
...
Wants=...cube-sandbox-cubeops.service
...
```

### 4.6 环境变量（`.one-click.env`）

在 `env.example` 中新增 CubeOps 配置段：

```bash
# ---- CubeOps (admin/ops API) ----
CUBE_OPS_BIND=127.0.0.1:3010
CUBE_OPS_LOG_LEVEL=info
CUBE_OPS_LOG_DIR=/data/log/CubeOps
# JWT_SECRET 留空时自动生成并持久化到 DB（单实例推荐）
# 多实例或 TKE 模式下显式设置：
# JWT_SECRET=
JWT_ACCESS_TTL=15m
JWT_REFRESH_TTL=168h
# CubeAPI URL（SDK 代理目标，默认同机）
CUBE_API_URL=http://127.0.0.1:3000
```

`DATABASE_URL` 和 `CUBE_MASTER_ADDR` 复用已有变量，无需新增。

## 5. WebUI / Nginx 路由变更

### 5.1 问题

CubeAPI 剥离运维逻辑后，WebUI 原先调用的 `/cubeapi/v1/*` admin 端点（AgentHub、Cluster、Auth 等）已不存在。需要将运维 API 请求路由到 CubeOps。

### 5.2 方案：nginx 双上游

修改 `deploy/one-click/webui/nginx.conf`，新增 `/opsapi/` location（运维 API）和 SDK 路径 rewrite（SDK 直连 CubeOps）：

```nginx
# 运维 API → CubeOps (:3010)
location /opsapi/ {
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;

    # 剥离 /opsapi 前缀并加上 /api/，CubeOps 路由为 /api/v1/*
    # e.g. /opsapi/v1/auth/login → /api/v1/auth/login
    proxy_pass http://127.0.0.1:3010/api/;
}

# SDK 路径 → CubeOps (:3010，JWT 鉴权 + SPA fallback)
# 浏览器导航（无 Authorization 头）返回 SPA HTML；API 调用（有 JWT）转发到 CubeOps
location ~ ^/(sandboxes|v2/sandboxes|templates|snapshots) {
    if ($http_authorization = "") {
        return 418;
    }
    add_header Vary "Authorization" always;
    add_header Cache-Control "no-store" always;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;

    rewrite ^/(.*)$ /api/v1/sdk/$1 break;
    proxy_pass http://127.0.0.1:3010;
}

error_page 418 = @spa_fallback;
location @spa_fallback {
    root /usr/share/nginx/html;
    try_files /index.html =404;
    add_header Vary "Authorization" always;
    add_header Cache-Control "no-store, no-cache, must-revalidate" always;
}
```

> **注意**：CubeOps 的 SDK 接口（`/api/v1/sdk/*`）直接调用 CubeMaster HTTP API，**不经过 CubeAPI**。CubeAPI 仅服务于外部 SDK 客户端（开发者代码直接调用 `:3000`）。

### 5.3 WebUI 前端适配

WebUI 前端使用双 base URL（`web/src/lib/api.ts`）：

| 前端模块 | 调用函数 | 实际路径 | nginx 转发目标 |
|----------|----------|----------|----------------|
| 登录/认证 | `ops()` | `/opsapi/v1/auth/*` | CubeOps `/api/v1/auth/*` |
| AgentHub 管理 | `ops()` | `/opsapi/v1/agenthub/*` | CubeOps `/api/v1/agenthub/*` |
| 集群/节点 | `ops()` | `/opsapi/v1/cluster/*` | CubeOps `/api/v1/cluster/*` |
| 镜像元数据 | `ops()` | `/opsapi/v1/store/*` | CubeOps `/api/v1/store/*` |
| 运行时配置 | `ops()` | `/opsapi/v1/config` | CubeOps `/api/v1/config` |
| SDK 操作（沙箱/模板列表等） | `api()` | `/sandboxes`、`/templates` | nginx rewrite → CubeOps `/api/v1/sdk/*` |

> SDK 操作通过 nginx rewrite 到 CubeOps 的 `/api/v1/sdk/*`，由 CubeOps 直接调 CubeMaster，不经过 CubeAPI。

### 5.4 旧路径兼容（可选）

升级过渡期可保留 `/cubeapi/v1/` 旧路径的 rewrite（已在当前 nginx 配置中存在）：

```nginx
# 兼容旧 /cubeapi/v1/ SDK 路径 → rewrite 到 CubeOps /api/v1/sdk/
location /cubeapi/v1/ {
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    rewrite ^/cubeapi/v1/(.*)$ /api/v1/sdk/$1 break;
    proxy_pass http://127.0.0.1:3010;
}
```

## 6. 数据库共享与迁移

### 6.1 cubedb 部署形态

`cubedb` **不是独立部署的服务**——它没有进程、没有端口、没有 systemd unit。它是一个纯 Go library（module），被 CubeMaster 和 CubeOps 作为依赖 `import`，在编译时链入各自二进制。

关键机制：`cubedb/migrate/migrate.go` 使用 `//go:embed migrations/mysql/*.sql` 将全部 SQL 迁移文件**编译时嵌入二进制**。因此：

| 维度 | 说明 |
|------|------|
| **运行时** | 无独立进程，迁移逻辑内嵌在 CubeMaster / CubeOps 二进制中 |
| **构建时** | 作为 Go module 被 import，通过 `go.work` workspace 或 `replace ../cubedb` 引入 |
| **迁移文件分发** | **不需要在目标机器上单独部署 SQL 文件**——已 embed 进二进制 |
| **安装侧改动** | 无——不需要在 `install.sh` 中为 cubedb 做任何操作 |

构建时的依赖链：

```
go.work
├── CubeMaster/go.mod  → require cubedb (replace ../cubedb)
├── CubeOps/go.mod     → require cubedb (replace ../cubedb)
└── cubedb/go.mod      → require goose, gorm, mysql driver
```

CubeMaster 和 CubeOps 编译时，Go 工具链自动拉取 `cubedb` 源码（本地 replace 路径），`go:embed` 指令将 `migrations/mysql/*.sql` 打包进最终二进制。目标机器上只需一个编译好的二进制文件，迁移即可自举。

### 6.2 共享模型

CubeOps 和 CubeMaster 共用同一 MySQL 实例和数据库（`cube_mvp`），通过 `cubedb` 共享模块管理 schema 迁移：

```
cubedb/migrate/migrations/mysql/        ← 源码仓库中的迁移文件
├── 0001_baseline_v0_2_2.sql            （编译时 embed 进二进制）
├── 0002_v0_2_2_to_head.sql
├── 0003_agenthub_instances.sql
├── ...
└── 0009_agenthub_openclaw_persistence_fields.sql
```

`cubedb` 使用 goose + 内容指纹篡改检测 + 集群级锁，确保多服务启动时迁移不会冲突。

### 6.3 迁移执行顺序

```
CubeMaster 启动 → cubedb 迁移（如有新 migration）
CubeOps 启动    → cubedb 迁移（幂等，已应用的跳过）
```

两者都调用 `cubedb` 的 `dao.New()` → `migrate.Up()`，迁移本身是幂等的（goose 版本表）。`cubedb` 的集群级锁确保并发安全。

### 6.4 AgentHub 表归属

AgentHub 相关表（`agenthub_instances`、`agenthub_snapshots`、`agenthub_templates`、`agenthub_operations`、`agenthub_settings`）原由 CubeAPI 的 Rust SQLx 管理，现由 CubeOps 的 Go GORM 管理。迁移文件不变（同一 SQL），ORM 层切换不影响 schema。

### 6.5 JWT Secret 持久化

CubeOps 首次启动时，若 `JWT_SECRET` 环境变量未设置，则：
1. 生成 32 字节随机 secret
2. AES-GCM 加密后存入 `t_system_setting` 表（`jwt_secret` key）
3. 后续启动从 DB 读取

> **注意**：系统配置已从 AgentHub 表分离——`jwt_secret`、`secret_master_key` 存在 `t_system_setting`，用户账号存在 `t_system_user`，AgentHub 业务配置存在 `t_agenthub_setting`。迁移文件 `20260713160000_system_setting_table.sql` 负责从旧表迁移数据。

多实例部署时需显式设置 `JWT_SECRET` 环境变量，确保所有实例使用同一签名密钥。

## 7. 安全考量

### 7.1 网络隔离

| 组件 | 绑定地址 | 暴露方式 |
|------|----------|----------|
| CubeOps | `127.0.0.1:3010` | 仅本机，通过 WebUI nginx 反代 |
| CubeAPI | `0.0.0.0:3000` | 公网（SDK 客户端访问） |
| CubeMaster | `0.0.0.0:8089` | 内网（计算节点 + CubeOps 访问） |

CubeOps **不得**绑定 `0.0.0.0`。所有外部访问必须经过 WebUI nginx（`:12088`），nginx 负责：
- TLS 终止（可选）
- 请求路由
- 日志审计

### 7.2 认证链路

**WebUI 运维 API（JWT 认证）：**
```
浏览器 → WebUI nginx /opsapi/v1/* → CubeOps /api/v1/* (JWT Bearer)
                                        ↓ JWT 验证通过
                                     CubeOps handler → CubeMaster HTTP API
```

**WebUI SDK API（JWT 认证，直连 CubeMaster）：**
```
浏览器 → WebUI nginx /sandboxes → rewrite → CubeOps /api/v1/sdk/sandboxes (JWT)
                                                  ↓ JWT 验证通过
                                               CubeOps handler → CubeMaster HTTP API（直接调用，不经过 CubeAPI）
```

**外部 SDK 客户端（CubeAPI 独立认证）：**
```
E2B SDK → CubeAPI :3000 /sandboxes (X-API-Key 或 Authorization: Bearer)
                    ↓ CUBE_API_KEY 字符串比较 或 AUTH_CALLBACK_URL 回调
                 CubeAPI handler → CubeMaster HTTP API
```

> CubeOps 和 CubeAPI **各自独立认证**，互不依赖。CubeOps 用 JWT，CubeAPI 用 `CUBE_API_KEY` 或 `AUTH_CALLBACK_URL`。

### 7.3 默认凭证

- 首次启动自动创建 `admin` / `admin` 账户
- **部署后必须立即修改默认密码**（`POST /api/v1/auth/change-password`）
- 建议在 `install.sh` 的 postcheck 阶段提示用户修改

### 7.4 CubeMaster→CubeAPI 鉴权适配

CubeAPI 的 `AUTH_CALLBACK_URL` 可指向 CubeOps 的 session 校验端点，实现 SDK 请求的统一鉴权：

```bash
# .one-click.env
AUTH_CALLBACK_URL=http://127.0.0.1:3010/api/v1/auth/session
```

CubeAPI 收到 SDK 请求时，POST 到 CubeOps 校验 JWT 有效性，200 放行、否则 401。这样 SDK 请求也纳入 CubeOps 的认证体系。

> 注意：此为可选增强。当前 CubeOps 的 `/api/v1/sdk/*` 代理已在 CubeOps 侧完成 JWT 校验，CubeAPI 默认不开启 auth callback 也可工作。

## 8. 升级路径

### 8.1 从旧版（CubeAPI 含运维逻辑）升级

```
旧版节点                          新版节点
─────────                        ─────────
CubeAPI (含 agenthub/auth/...)   CubeAPI (纯 SDK)
                                 CubeOps (运维逻辑)
                                 cubedb (共享迁移)
```

升级步骤：

1. **构建新发布包**：包含 CubeOps 二进制 + 更新后的 CubeAPI 二进制（已删除运维 handler）
2. **停止旧服务**：`sudo ./down.sh`
3. **安装新包**：`sudo ./install.sh`
   - 替换 CubeAPI 二进制
   - 部署 CubeOps 二进制
   - 安装新的 systemd service 文件
   - 安装 cubedb 迁移文件（如有新增）
4. **数据库迁移**：CubeOps 首次启动时自动执行 `cubedb` 迁移
5. **WebUI 更新**：前端 API 路径切换到 `/opsapi/v1/*`（运维）和 SDK 直连路径（`/sandboxes` 等）
6. **验证**：`sudo ./smoke.sh`

### 8.2 数据兼容

- AgentHub 表结构不变，数据无需迁移
- 原有的 admin 账户（如已通过旧 CubeAPI 创建）保留（迁移到 `t_system_user` 表）
- JWT secret 首次启动时生成，存入 `t_system_setting` 表（不影响已有 AgentHub 数据）
- 系统配置（`jwt_secret`、`secret_master_key`）从 `t_agenthub_setting` 迁移到 `t_system_setting`，用户账号从 `t_agenthub_user` 迁移到 `t_system_user`（迁移文件 `20260713160000_system_setting_table.sql` 自动执行）

### 8.3 回滚

如需回滚到旧版：
1. `sudo ./down.sh`
2. 恢复旧发布包
3. `sudo ./install.sh`
4. 旧 CubeAPI 的运维 handler 重新可用，CubeOps 不启动（旧 systemd service 不存在）

数据库 schema 向后兼容（cubedb 迁移为加列/加表，不删列）。

## 9. 集群模式（Terraform/TKE）

### 9.1 Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cube-ops
  namespace: cube-sandbox
spec:
  replicas: 1                      # 单副本，JWT secret 无需共享
  selector:
    matchLabels:
      app: cube-ops
  template:
    metadata:
      labels:
        app: cube-ops
    spec:
      containers:
      - name: cube-ops
        image: cube-ops:${TAG}
        ports:
        - containerPort: 3010
        env:
        - name: CUBE_OPS_BIND
          value: "0.0.0.0:3010"    # TKE 内绑定 0.0.0.0，由 Service/NetworkPolicy 隔离
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              name: cube-db-credentials
              key: database-url
        - name: CUBE_MASTER_ADDR
          value: "http://cubemaster:8089"   # ClusterIP Service
        - name: CUBE_API_URL
          value: "http://cubeapi:3000"      # ClusterIP Service
        - name: JWT_SECRET
          valueFrom:
            secretKeyRef:
              name: cube-ops-secrets
              key: jwt-secret
        - name: REDIS_URL
          valueFrom:
            secretKeyRef:
              name: cube-redis-credentials
              key: redis-url
        livenessProbe:
          httpGet:
            path: /health
            port: 3010
          initialDelaySeconds: 5
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 3010
          initialDelaySeconds: 3
          periodSeconds: 5
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: cubeops
  namespace: cube-sandbox
spec:
  selector:
    app: cube-ops
  ports:
  - port: 3010
    targetPort: 3010
  type: ClusterIP            # 仅集群内访问
```

### 9.2 多副本注意事项

若 `replicas > 1`：
- **必须显式设置 `JWT_SECRET`**（所有副本共享同一签名密钥）
- **必须配置 Redis**（`REDIS_URL`），用于 JWT 黑名单同步（logout 失效）
- AgentHub 操作（创建/删除/快照等）通过 CubeMaster 的分布式锁保证一致性，CubeOps 本身无状态

### 9.3 NetworkPolicy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cube-ops-policy
  namespace: cube-sandbox
spec:
  podSelector:
    matchLabels:
      app: cube-ops
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: webui          # 仅 WebUI Pod 可访问
    ports:
    - protocol: TCP
      port: 3010
  egress:
  - to:
    - podSelector:
        matchLabels:
          app: cubemaster
    ports:
    - protocol: TCP
      port: 8089
  - to:
    - podSelector:
        matchLabels:
          app: cubeapi
    ports:
    - protocol: TCP
      port: 3000
  # MySQL/Redis 通过云内网 VPC 访问
```

### 9.4 Terraform 集成

在 `terraform/tencentcloud/` 的 TKE addon manifest 中新增 CubeOps Deployment + Service。镜像来源：
- 默认模式（`TENCENTCLOUD_USE_TCR=false`）：使用公网预构建镜像
- TCR 模式（`TENCENTCLOUD_USE_TCR=true`）：jumpserver 构建推送到 TCR

## 10. 冒烟测试

### 10.1 one-click smoke.sh 新增检查

```bash
# cubeops 健康检查
cubeops_health() {
    local url="http://127.0.0.1:${CUBE_OPS_BIND##*:}/health"
    if curl -sf --max-time 5 "${url}" >/dev/null 2>&1; then
        echo "  [OK] cubeops healthy"
    else
        echo "  [FAIL] cubeops unhealthy"
        return 1
    fi
}

# cubeops 认证检查
cubeops_auth() {
    local resp
    resp=$(curl -sf --max-time 5 \
        -X POST "http://127.0.0.1:${CUBE_OPS_BIND##*:}/api/v1/auth/login" \
        -H 'Content-Type: application/json' \
        -d '{"username":"admin","password":"admin"}' 2>/dev/null) || true

    if echo "${resp}" | grep -q "accessToken"; then
        echo "  [OK] cubeops auth login works"
    else
        echo "  [WARN] cubeops auth login failed (expected if password changed)"
    fi
}
```

### 10.2 端到端验证清单

| # | 检查项 | 命令 |
|---|--------|------|
| 1 | CubeOps 进程运行 | `systemctl status cube-sandbox-cubeops` |
| 2 | 健康端点 | `curl http://127.0.0.1:3010/health` |
| 3 | 登录获取 JWT | `curl -X POST http://127.0.0.1:3010/api/v1/auth/login -d '{"username":"admin","password":"admin"}'` |
| 4 | 集群概览 | `curl -H "Authorization: Bearer <token>" http://127.0.0.1:3010/api/v1/cluster/overview` |
| 5 | AgentHub 实例列表 | `curl -H "Authorization: Bearer <token>" http://127.0.0.1:3010/api/v1/agenthub/instances` |
| 6 | SDK 代理 | `curl -H "Authorization: Bearer <token>" http://127.0.0.1:3010/api/v1/sdk/sandboxes` |
| 7 | WebUI 路由 | `curl http://<host>:12088/cubeops/api/v1/auth/login -X POST -d '{"username":"admin","password":"admin"}'` |
| 8 | 修改默认密码 | `curl -X POST -H "Authorization: Bearer <token>" http://127.0.0.1:3010/api/v1/auth/change-password -d '{"old_password":"admin","new_password":"<new>"}'` |

## 11. install.sh 改动清单

以下是需要对现有 one-click 安装脚本做的改动汇总：

### 11.1 构建侧

| 文件 | 改动 |
|------|------|
| `build-release-bundle-builder.sh` | 编译清单新增 CubeOps（`cd CubeOps && go build`） |
| `build-release-bundle.sh` | 打包 `CubeOps/bin/cubeops` 到 sandbox-package |
| `env.example` | 新增 `ONE_CLICK_CUBE_OPS_BUILD_MODE`、`ONE_CLICK_CUBE_OPS_BIN`、CubeOps 运行时配置段 |

### 11.2 安装侧

| 文件 | 改动 |
|------|------|
| `install.sh` | 部署 CubeOps 二进制到 `CubeOps/bin/`；安装 systemd service 文件；注册到 control target |
| `systemd/cube-sandbox-cubeops.service` | **新增** |
| `systemd/cube-sandbox-control.target` | `Wants=` 新增 `cube-sandbox-cubeops.service` |
| `scripts/systemd/cubeops-start.sh` | **新增** |
| `scripts/systemd/cubeops-postcheck.sh` | **新增** |
| `scripts/one-click/quickcheck.sh` | 新增 cubeops 健康检查 |
| `scripts/one-click/smoke.sh` | 新增 cubeops 冒烟测试 |
| `down.sh` | systemd 会随 control.target 统一停止，无需单独改动 |
| `webui/nginx.conf` | 新增 `/opsapi/` location + SDK 路径 rewrite |
| `webui/docker-compose.yaml.template` | 无需改动（nginx 配置外部挂载） |

### 11.3 WebUI 前端

| 模块 | 改动 |
|------|------|
| API client base URL | 运维接口从 `/cubeapi/v1` → `/opsapi/v1`（`ops()` 函数） |
| SDK 接口 | 从 `/cubeapi/v1/sandboxes` → `/sandboxes`（`api()` 函数，nginx rewrite 到 `/api/v1/sdk/sandboxes`） |
| 登录页 | API 路径更新 |
| `/keys` 页面 | 已删除（CubeOps 不认 `X-API-Key`，该页面是旧 CubeAPI 时代遗留） |

## 12. 资源规划

### 12.1 All-in-One 模式

| 资源 | CubeOps 需求 | 说明 |
|------|-------------|------|
| CPU | 0.1~0.5 核 | Go HTTP 服务，低并发 |
| 内存 | 50~128 MB | GORM + gorilla/mux |
| 磁盘 | <10 MB（二进制） + 日志 | 日志按天滚动 |
| 网络 | 1 个 localhost 端口 | `:3010` |

对 All-in-One 节点的总体资源影响可忽略不计。

### 12.2 TKE 模式

| 资源 | requests | limits |
|------|----------|--------|
| CPU | 100m | 500m |
| 内存 | 128Mi | 512Mi |

单副本足以支撑运维后台的并发需求。如需高可用，可扩到 2 副本（需配置共享 `JWT_SECRET` + Redis）。

## 附录 A：端口分配总表

| 端口 | 组件 | 绑定 | 用途 | 认证 |
|------|------|------|------|------|
| 3000 | CubeAPI | `0.0.0.0` | E2B SDK API（面向外部 SDK 客户端） | `CUBE_API_KEY` 或 `AUTH_CALLBACK_URL` |
| 3010 | CubeOps | `127.0.0.1` | 运维 API + SDK 直连 CubeMaster（WebUI 用） | JWT Bearer |
| 8089 | CubeMaster | `0.0.0.0` | gRPC + HTTP 调度 | 内网信任 |
| 9999 | Cubelet | `0.0.0.0` | gRPC 节点通信 | 内网信任 |
| 80/443 | CubeProxy | `0.0.0.0` | 沙箱流量路由 + TLS | — |
| 12088 | WebUI | `0.0.0.0` | 管理控制台（nginx 反代 CubeOps） | — |
| 3306 | MySQL | `127.0.0.1` | 共享数据库（CubeMaster + CubeOps） | DB 密码 |
| 6379 | Redis | `127.0.0.1` | 生命周期事件 | 密码（可选） |
| 19090 | network-agent | `127.0.0.1` | 健康检查 | — |

## 附录 B：环境变量总表

| 变量 | 默认值 | 来源 | 说明 |
|------|--------|------|------|
| `CUBE_OPS_BIND` | `127.0.0.1:3010` | env | 监听地址 |
| `CUBE_OPS_LOG_LEVEL` | `info` | env | 日志级别 |
| `CUBE_OPS_LOG_DIR` | `/data/log/CubeOps` | start.sh | 日志目录 |
| `JWT_SECRET` | *(auto)* | env/DB | JWT 签名密钥 |
| `JWT_ACCESS_TTL` | `15m` | env | Access token 有效期 |
| `JWT_REFRESH_TTL` | `168h` | env | Refresh token 有效期 |
| `DATABASE_URL` | *(from MySQL vars)* | env | MySQL 连接 URL |
| `CUBE_MASTER_ADDR` | `http://127.0.0.1:8089` | env | CubeMaster 地址（CubeOps 和 CubeAPI 共用） |
| `REDIS_URL` | *(optional)* | env | Redis（JWT 黑名单） |
| `CUBE_SANDBOX_MYSQL_*` | *(见 env.example)* | env | MySQL 连接参数（构建 DATABASE_URL） |
| `CUBE_SANDBOX_REDIS_*` | *(见 env.example)* | env | Redis 连接参数 |
| `CUBE_API_KEY` | *(unset)* | env | CubeAPI 内置简单认证密钥（unset=无认证） |
| `CUBE_OPS_PUBLIC_HOST` | *(from bind)* | env | CubeOps 对外公网地址（Settings 页展示） |
| `CUBE_API_PUBLIC_HOST` | *(from bind)* | env | CubeAPI 对外公网地址（E2B SDK 客户端用） |

## 13. 实际部署操作步骤

> 以下步骤基于当前代码实际实现，适用于 All-in-One 模式的手动部署验证。

### 13.1 前提条件

```bash
# 已安装并运行的服务
systemctl status cube-sandbox-cubemaster   # CubeMaster :8089
systemctl status cube-sandbox-mysql        # MySQL :3306
docker ps | grep cube-webui               # WebUI nginx :12088
docker ps | grep cube-sandbox-redis       # Redis :6379

# 确认 Go 和 Rust 工具链
go version    # >= 1.21
cargo --version
node --version  # >= 18 (前端构建)
```

### 13.2 构建 CubeOps 二进制

```bash
cd /root/CubeSandbox/CubeOps

# 编译 release 二进制
go build -o bin/cubeops ./cmd/cubeops

# 验证
./bin/cubeops --help 2>&1 | head -5
# 或直接启动看日志
CUBE_OPS_BIND=127.0.0.1:3010 \
CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp \
CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK=1 \
./bin/cubeops
# 看到 "CubeOps starting" 日志即为成功，Ctrl+C 退出
```

### 13.3 构建 CubeAPI 二进制

```bash
cd /root/CubeSandbox/CubeAPI

# 编译 release 二进制（含 CUBE_API_KEY 认证功能）
cargo build --release

# 验证
CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
CUBE_API_KEY=e2b_0000000000000000000000000000000000000000 \
./target/release/cube-api
# 看到 "cube-api listening on 0.0.0.0:3000" 和 "auth_enabled=true" 即为成功
```

### 13.4 构建前端

```bash
cd /root/CubeSandbox/web

# 安装依赖（首次）
npm install

# 构建
npm run build

# 产物在 dist/ 目录
ls dist/assets/index-*.js
```

### 13.5 部署前端到 WebUI 容器

```bash
# 将构建产物复制到 WebUI nginx 的静态文件目录
docker cp dist/. cube-webui:/usr/share/nginx/html/

# 或挂载宿主机目录（推荐，重启不丢）
# 修改 docker-compose.yaml 挂载 /root/CubeSandbox/web/dist 到 /usr/share/nginx/html
```

### 13.6 配置 nginx

编辑 `/usr/local/services/cubetoolbox/webui/nginx.conf`（或 nginx.generated.conf），确保以下 location 配置：

```nginx
# CubeOps 运维 API → :3010
location /opsapi/ {
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    proxy_pass http://host.docker.internal:3010/api/;
}

# SDK 路径 → CubeOps :3010（带 JWT 鉴权 + SPA fallback）
location ~ ^/(sandboxes|v2/sandboxes|templates|snapshots) {
    if ($http_authorization = "") {
        return 418;
    }
    add_header Vary "Authorization" always;
    add_header Cache-Control "no-store" always;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    rewrite ^/(.*)$ /api/v1/sdk/$1 break;
    proxy_pass http://host.docker.internal:3010;
}

error_page 418 = @spa_fallback;
location @spa_fallback {
    root /usr/share/nginx/html;
    try_files /index.html =404;
    add_header Vary "Authorization" always;
    add_header Cache-Control "no-store, no-cache, must-revalidate" always;
}

# 旧 /cubeapi/v1/ 兼容路径 → CubeOps（可选，保留不影响）
location /cubeapi/v1/ {
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    rewrite ^/cubeapi/v1/(.*)$ /api/v1/sdk/$1 break;
    proxy_pass http://host.docker.internal:3010;
}

# CubeAPI health → :3000（CubeAPI 独立运行时）
location = /health {
    proxy_pass http://host.docker.internal:3000;
}
```

重新加载 nginx：

```bash
# 如果是 Docker 中的 nginx
docker exec cube-webui nginx -s reload

# 如果是宿主机 nginx
nginx -s reload
```

### 13.7 配置 systemd（生产部署）

#### 13.7.1 CubeOps service 文件

创建 `/etc/systemd/system/cube-sandbox-cubeops.service`：

```ini
[Unit]
Description=Cube Sandbox CubeOps (admin/ops API)
After=network-online.target cube-sandbox-cubemaster.service
Wants=network-online.target cube-sandbox-cubemaster.service
ConditionPathExists=/usr/local/services/cubetoolbox/.one-click.env
PartOf=cube-sandbox-control.target

[Service]
Type=simple
User=root
WorkingDirectory=/usr/local/services/cubetoolbox
EnvironmentFile=-/usr/local/services/cubetoolbox/.one-click.env
ExecStart=/usr/bin/bash /usr/local/services/cubetoolbox/scripts/systemd/cubeops-start.sh
ExecStartPost=/usr/bin/bash /usr/local/services/cubetoolbox/scripts/systemd/cubeops-postcheck.sh
Restart=on-failure
RestartSec=2s
TimeoutStartSec=60s
TimeoutStopSec=30s

[Install]
WantedBy=cube-sandbox-control.target
```

#### 13.7.2 CubeOps 启动脚本

创建 `/usr/local/services/cubetoolbox/scripts/systemd/cubeops-start.sh`：

```bash
#!/usr/bin/env bash
set -euo pipefail

CUBE_OPS_BIN="/usr/local/services/cubetoolbox/CubeOps/bin/cubeops"

export CUBE_OPS_BIND="${CUBE_OPS_BIND:-127.0.0.1:3010}"
export CUBE_MASTER_ADDR="${CUBE_MASTER_ADDR:-http://127.0.0.1:8089}"

if [[ -z "${DATABASE_URL:-}" ]]; then
  export DATABASE_URL="mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp"
fi

# 可选：公网地址展示（Settings 页面）
# export CUBE_OPS_PUBLIC_HOST="your-public-host:12088"

exec "${CUBE_OPS_BIN}"
```

#### 13.7.3 CubeAPI service 文件

创建/更新 `/etc/systemd/system/cube-sandbox-cube-api.service`：

```ini
[Unit]
Description=Cube Sandbox CubeAPI (E2B SDK API)
After=network-online.target cube-sandbox-cubemaster.service
Wants=network-online.target cube-sandbox-cubemaster.service
PartOf=cube-sandbox-control.target

[Service]
Type=simple
User=root
WorkingDirectory=/usr/local/services/cubetoolbox
EnvironmentFile=-/usr/local/services/cubetoolbox/.one-click.env
ExecStart=/usr/local/services/cubetoolbox/CubeAPI/bin/cube-api
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=cube-sandbox-control.target
```

#### 13.7.4 注册到 control target

在 `/etc/systemd/system/cube-sandbox-control.target` 的 `Wants=` 中添加：

```
Wants=...cube-sandbox-cubeops.service cube-sandbox-cube-api.service
```

#### 13.7.5 启用服务

```bash
chmod +x /usr/local/services/cubetoolbox/scripts/systemd/cubeops-start.sh

systemctl daemon-reload
systemctl enable cube-sandbox-cubeops cube-sandbox-cube-api

# 启动
systemctl start cube-sandbox-cubeops
systemctl start cube-sandbox-cube-api

# 查看状态
systemctl status cube-sandbox-cubeops
systemctl status cube-sandbox-cube-api
```

### 13.8 环境变量配置

编辑 `/usr/local/services/cubetoolbox/.one-click.env`，添加：

```bash
# ---- CubeOps ----
CUBE_OPS_BIND=127.0.0.1:3010
CUBE_MASTER_ADDR=http://127.0.0.1:8089
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
# JWT_SECRET 留空时自动生成并持久化到 DB
# JWT_SECRET=
# CUBE_OPS_PUBLIC_HOST=your-public-host:12088

# ---- CubeAPI ----
CUBE_API_BIND=0.0.0.0:3000
CUBE_MASTER_ADDR=http://127.0.0.1:8089
# E2B SDK 兼容格式的 API Key（unset=无认证模式）
CUBE_API_KEY=e2b_0000000000000000000000000000000000000000
```

### 13.9 手动启动（非 systemd，用于验证）

```bash
# CubeOps
cd /root/CubeSandbox/CubeOps
CUBE_OPS_BIND=0.0.0.0:3010 \
CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp \
CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK=1 \
./bin/cubeops &

# CubeAPI
cd /root/CubeSandbox/CubeAPI
CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
CUBE_API_BIND=0.0.0.0:3000 \
CUBE_API_KEY=e2b_0000000000000000000000000000000000000000 \
./target/release/cube-api &
```

### 13.10 部署后验证

```bash
# 1. CubeOps health
curl http://127.0.0.1:3010/health
# 期望: ok

# 2. CubeOps 登录
curl -s -X POST http://127.0.0.1:3010/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | python3 -m json.tool
# 期望: {"accessToken":"...","refreshToken":"..."}

# 3. WebUI 通过 nginx 登录
curl -s -X POST http://127.0.0.1:12088/opsapi/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | python3 -m json.tool

# 4. CubeAPI health
curl http://127.0.0.1:3000/health
# 期望: {"status":"ok","sandboxes":0}

# 5. CubeAPI 认证（如果设了 CUBE_API_KEY）
curl -sI http://127.0.0.1:3000/sandboxes
# 期望: HTTP/1.1 401（无凭证）

curl -H "X-API-Key: e2b_0000000000000000000000000000000000000000" \
  http://127.0.0.1:3000/sandboxes
# 期望: 200 + 沙箱列表

# 6. 浏览器访问 WebUI
# http://<host>:12088 → 用 admin/admin 登录
# 检查：
#   - 概览页能显示集群信息
#   - 模板列表能加载
#   - 沙箱列表能加载
#   - AgentHub 数字助手列表能加载
#   - 设置 → 集群连接 → API 端点显示 CubeOps 地址
```

### 13.11 二进制部署位置（生产）

```bash
# 创建目录
mkdir -p /usr/local/services/cubetoolbox/CubeOps/bin
mkdir -p /usr/local/services/cubetoolbox/CubeAPI/bin

# 复制二进制
cp /root/CubeSandbox/CubeOps/bin/cubeops /usr/local/services/cubetoolbox/CubeOps/bin/
cp /root/CubeSandbox/CubeAPI/target/release/cube-api /usr/local/services/cubetoolbox/CubeAPI/bin/

# 复制前端
cp -r /root/CubeSandbox/web/dist/* /usr/local/services/cubetoolbox/webui/dist/
# 或直接 docker cp 到容器
```

### 13.12 常见问题

| 问题 | 原因 | 解决 |
|------|------|------|
| CubeOps 启动报 `database connection refused` | MySQL 未启动或地址错误 | 检查 `DATABASE_URL` 和 MySQL 状态 |
| CubeOps 启动报 `cubemaster unreachable` | CubeMaster 未启动 | `systemctl status cube-sandbox-cubemaster` |
| WebUI 登录 404 | nginx `/opsapi/` 路径未配置 | 检查 nginx 配置并 `nginx -s reload` |
| WebUI 模板列表空 | nginx SDK 路径未配置或 JWT 过期 | 检查 `location ~ ^/(sandboxes\|templates)` 配置 |
| CubeAPI 401 | `CUBE_API_KEY` 设置后请求未带凭证 | 请求加 `X-API-Key` 或 `Authorization: Bearer` 头 |
| 沙箱登录环境跳转到错误端口 | `env_port` 数据库存错 | 见 `store/agenthub.go` 的 `UpsertInstance` 逻辑 |
| Settings 页 API 端点显示 127.0.0.1 | 未设 `CUBE_OPS_PUBLIC_HOST` | 启动时设 `CUBE_OPS_PUBLIC_HOST=<公网IP>:12088` |
