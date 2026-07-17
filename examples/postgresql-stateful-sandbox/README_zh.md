# PostgreSQL 有状态沙箱

[English](README.md)

在 CubeSandbox MicroVM 中运行一个自包含的 PostgreSQL 16.14 有状态服务。数据库只接受
本地 Unix Socket 连接；宿主端 Python 驱动通过 Cube 已鉴权的 `envd` 命令通道执行 SQL。

本示例提供三个完整流程：

- `smoke.py`：创建数据库、执行 SQL，并验证入站和出站网络限制。
- `snapshot_restore.py`：为运行中的数据库创建检查点，执行破坏性数据和 schema 变更，
  再把 snapshot 恢复到新沙箱。
- `migration_branches.py`：从同一个数据库快照创建两条独立 migration 分支，证明文件、
  schema 和 migration ledger 相互隔离。

该镜像适用于开发、测试、migration 演练和 Agent 工作负载，不是生产 PostgreSQL 部署或
备份系统。

## 架构与安全模型

```text
宿主端 Python 脚本
    |
    | cubesandbox SDK（每个沙箱独立的流量令牌）
    v
CubeProxy -> envd :49983 -> 以 postgres 系统用户执行命令
                                  |
                                  v
                         PostgreSQL Unix Socket
                         /var/run/postgresql
```

容器进程树：

```text
tini
`-- cube-entrypoint.sh
    |-- postgres-ready-envd.sh
    |   `-- 等待 pg_isready，再启动 envd :49983
    `-- start-postgres.sh
        `-- postgres（前台进程）
```

就绪端点会等待 `pg_isready` 成功后才启动。因此，`:49983/health` 返回 HTTP 204 表示
Cube 命令通道和 PostgreSQL 都已经就绪。

数据库边界有意保持最小：

- PostgreSQL 不监听任何 TCP 地址；Cube 模板不登记 5432 端口。
- 本地连接使用 `peer` 鉴权，`pg_hba.conf` 拒绝 host 连接。
- 不创建或保存 PostgreSQL 密码。
- 每个沙箱和快照分支都禁止公网出站。
- `network.allow_public_traffic=false` 使用每个沙箱独立的令牌保护入站 `envd` 流量，
  原生 SDK 会自动携带该令牌。

沙箱内的 root 可以切换到 `postgres` 系统用户。因此，安全边界是 MicroVM，而不是
PostgreSQL role；不要让互不信任的用户共享同一个实例。

## 目录结构

```text
postgresql-stateful-sandbox/
|-- Dockerfile                 # PostgreSQL 16.14 + Cube envd 模板镜像
|-- .dockerignore
|-- cube-entrypoint.sh         # 等待 PostgreSQL 完成干净退出
|-- start-postgres.sh          # 仅 Unix Socket 的 PostgreSQL 前台进程
|-- postgres-ready-envd.sh     # 数据库就绪后才启动 envd
|-- .env.example               # 宿主端 SDK 配置
|-- requirements.txt           # cubesandbox 和 python-dotenv
|-- env_utils.py               # 加载并校验 .env
|-- postgres_utils.py          # SQL、快照和清理公共辅助函数
|-- smoke.py                   # 最小 SQL 与网络隔离验证
|-- snapshot_restore.py        # 数据 + schema 恢复演示
|-- migration_branches.py      # 同一快照派生两条隔离分支
|-- sql/
|   |-- base_schema.sql        # 账户表、种子数据、migration ledger
|   |-- add_email.sql          # 分支 A migration
|   `-- add_last_login.sql     # 分支 B migration
|-- README.md
`-- README_zh.md
```

## 前置条件

- 已部署 CubeSandbox，且宿主机可以访问 CubeAPI。
- `cubemastercli` 已安装到 `PATH` 并连接 CubeMaster。
- Docker 支持构建 `linux/amd64` 镜像。
- 所有目标 Cube 节点都可以拉取的 OCI registry。
- 宿主端驱动脚本使用 Python 3.9 或更高版本。
- 每个沙箱需要 2 vCPU 和 2 GiB 内存。migration 分支演示会同时运行两个沙箱，
  因此至少预留 4 vCPU、4 GiB 内存以及平台自身开销。

## 1. 本地构建并验证镜像

构建唯一支持的镜像：

```bash
export LOCAL_IMAGE=cubesandbox-postgresql-stateful:16.14

docker build --platform linux/amd64 \
  --tag "$LOCAL_IMAGE" \
  examples/postgresql-stateful-sandbox
```

启动镜像并等待 envd/PostgreSQL 组合健康检查：

```bash
docker run --detach \
  --name cubesandbox-postgresql-stateful \
  --publish 49983:49983 \
  "$LOCAL_IMAGE"

status=starting
attempt=0
while [ "$attempt" -lt 60 ]; do
  status=$(docker inspect --format '{{.State.Health.Status}}' \
    cubesandbox-postgresql-stateful)
  [ "$status" = healthy ] && break
  if [ "$status" = unhealthy ]; then
    docker logs cubesandbox-postgresql-stateful
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 2
done

if [ "$status" != healthy ]; then
  docker logs cubesandbox-postgresql-stateful
  echo 'ERROR: container did not become healthy within 120 seconds' >&2
  exit 1
fi

curl --fail --silent --show-error \
  --output /dev/null \
  --write-out 'envd health: %{http_code}\n' \
  http://127.0.0.1:49983/health
```

最后一条命令必须打印 `envd health: 204`。验证数据库版本、数据目录和 Unix Socket：

```bash
docker exec --user postgres cubesandbox-postgresql-stateful \
  psql -X -h /var/run/postgresql -U postgres -d postgres \
  -Atqc 'SHOW server_version; SHOW data_directory; SELECT 1;'
```

预期值包括：

```text
16.14 ...
/var/lib/postgresql/cube-data
1
```

TCP 必须保持不可用：

```bash
if docker exec --user postgres cubesandbox-postgresql-stateful \
  pg_isready -h 127.0.0.1 -p 5432 -U postgres -d postgres; then
  echo 'ERROR: PostgreSQL unexpectedly accepted TCP connections' >&2
  exit 1
else
  echo 'OK: PostgreSQL TCP port is closed'
fi
```

最后验证优雅停止并删除本地容器：

```bash
docker stop --time 15 cubesandbox-postgresql-stateful
docker rm cubesandbox-postgresql-stateful
```

## 2. 推送镜像并注册 Cube 模板

把镜像标记到 Cube 节点可访问的 registry：

```bash
export REGISTRY_IMAGE=registry.example.com/cubesandbox-postgresql-stateful:16.14

docker tag "$LOCAL_IMAGE" "$REGISTRY_IMAGE"
docker push "$REGISTRY_IMAGE"
```

把 `registry.example.com` 替换成你的 registry，然后提交模板构建。`--detach` 会返回供后续
显式 watch 使用的 job ID：

```bash
cubemastercli tpl create-from-image \
  --image "$REGISTRY_IMAGE" \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --cpu 2000 \
  --memory 2048 \
  --allow-internet-access=false \
  --with-cube-ca=false \
  --detach
```

保存输出的 `job_id` 和 `template_id`，等待全部目标节点的副本就绪：

```bash
cubemastercli tpl watch --job-id <job_id>
cubemastercli tpl info --template-id <template_id> --json --include-request
cubemastercli tpl render --template-id <template_id> --json
```

任务达到 `READY` 且 `distribution` 显示所有目标节点 ready 后再继续。检查保存和渲染后的
请求，确认：

- 只公开容器端口 49983；
- 探针为 49983 上的 `GET /health`；
- CPU 为 2000 millicores、内存为 2048 MB；
- 可写层为 4 GiB；
- 禁止访问互联网。

模板级断网是纵深防御。包括从 snapshot 创建分支时，每个 Python 驱动也会显式传入
`allow_internet_access=False`。

## 3. 配置宿主端驱动

```bash
cd examples/postgresql-stateful-sandbox
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

编辑 `.env`：

```dotenv
CUBE_API_URL=http://127.0.0.1:3000
CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxx

# 宿主机无法解析 Cube 沙箱通配域名时设置以下变量。
# CUBE_PROXY_NODE_IP=<cube-proxy-node-ip>
# CUBE_PROXY_PORT_HTTP=80
# CUBE_SANDBOX_DOMAIN=cube.app
```

| 变量 | 必填 | 用途 |
|---|:---:|---|
| `CUBE_API_URL` | 是 | CubeAPI 控制面地址 |
| `CUBE_TEMPLATE_ID` | 是 | 第 2 步生成的模板 ID |
| `CUBE_PROXY_NODE_IP` | 否 | 直接连接 CubeProxy，不依赖通配域名解析 |
| `CUBE_PROXY_PORT_HTTP` | 否 | CubeProxy HTTP 端口，默认 `80` |
| `CUBE_SANDBOX_DOMAIN` | 否 | 沙箱路由域名，默认 `cube.app` |

这些示例使用原生 `cubesandbox` SDK，而不是 E2B SDK，因此不需要 `E2B_API_URL` 或
`E2B_API_KEY`。

## 4. 运行三个流程

在当前目录执行脚本。断言失败时脚本会抛出异常，只有全部检查通过后才打印一行 `OK:`。

### 最小 SQL 与网络隔离

```bash
python smoke.py
```

脚本创建基线 schema、验证 PostgreSQL 16.14，并检查两条种子账户及余额总和 300。随后确认
创建响应包含流量令牌、携带该令牌访问 envd health 返回 204，再确认访问 `example.com` 的
出站请求失败、本地 SQL 仍可执行，并确认 TCP 5432 关闭。

预期最后一行：

```text
OK: PostgreSQL template is ready and network isolation is enforced
```

### 恢复快照到新沙箱

```bash
python snapshot_restore.py
```

脚本加载基线，等待全部事务结束，执行 `CHECKPOINT` 并创建 snapshot。随后把两个账户的
余额改为 0、增加 `poisoned` 列并验证破坏状态，再销毁源沙箱。脚本从 snapshot 创建一个
新沙箱，并断言证明：

- 恢复沙箱的 ID 与源沙箱不同；
- 余额恢复为 100 和 200；
- `poisoned` 列消失；
- 显式创建的 snapshot 在清理阶段被删除。

预期最后一行：

```text
OK: snapshot restored PostgreSQL data and schema in a new sandbox
```

### 隔离的 migration 分支

```bash
python migration_branches.py
```

脚本为源数据库创建检查点和一个 snapshot，然后销毁源沙箱。接着从同一个 snapshot 并行
创建两个沙箱，并为每个分支完整应用入站和出站安全配置：

- 分支 A 应用 `add_email.sql`；
- 分支 B 应用 `add_last_login.sql`。

两个分支都把各自的 migration 上传为 `/tmp/migration.sql`。断言会证明文件内容、schema
字段和 `schema_migrations` 记录相互隔离。即使部分创建失败，清理逻辑也会先销毁所有已创建
的子沙箱，再删除共享 snapshot。

预期最后一行：

```text
OK: isolated PostgreSQL migration branches passed
```

脚本有意不调用 `Sandbox.clone()`：当前 helper 会用默认创建配置生成子沙箱。显式调用
`Sandbox.create(template=snapshot_id, ...)` 可以确保每条分支都应用 timeout 和两项网络限制。

## 快照一致性

Cube snapshot 会捕获暂停后的 MicroVM 内存和文件系统。本示例把 `PGDATA` 与 `pg_wal`
一起保存在 `/var/lib/postgresql/cube-data`，并避开上游镜像声明的 volume，确保完整数据库
集群都包含在 snapshot 中。

创建可复用数据库 snapshot 前，脚本会：

1. 等待应用事务提交；
2. 执行 PostgreSQL `CHECKPOINT`；
3. 仅在 checkpoint 成功后调用 `create_snapshot()`。

这样可以减少从 snapshot 启动恢复或创建分支后的 WAL 恢复工作，但不会让 Cube snapshot 变成生产
备份或时间点恢复归档。不要把 `pg_wal` 移出 `PGDATA`、创建外部 tablespace，或在不同
PostgreSQL 主版本之间复制这些 snapshot。

Snapshot 的生命周期独立于源沙箱，并且可能包含敏感数据。`with Sandbox.create(...)` 只会
销毁沙箱，不会删除 snapshot。因此，所有脚本都会显式调用 `Sandbox.delete_snapshot()`。

## 资源建议

| 资源 | 配置 | 说明 |
|---|---:|---|
| CPU | 2000 millicores | 每个沙箱 |
| 内存 | 2048 MB | 每个沙箱；更大数据集或查询需要提高 |
| 可写层 | 4 GiB | 保存 PostgreSQL 数据和 WAL |
| 并行分支容量 | 至少 4 vCPU / 4 GiB | 两个 2-vCPU/2-GiB 分支，不含平台自身开销 |

插入较大数据集时要监控可写层。除表和 WAL 外，PostgreSQL 还可能需要临时磁盘空间；每个
保留的 snapshot 也会消耗持久化镜像存储。

## 已知限制

- 镜像只包含 PostgreSQL 16.14，目标架构为 `linux/amd64`。
- 这是单节点临时开发数据库：不提供 HA、复制、PITR、外部 tablespace、TLS listener 或
  PostgreSQL 原生入站访问。
- 未创建 snapshot 就销毁沙箱会永久丢弃其状态。
- Snapshot 只保证与当前镜像和 PostgreSQL 主版本兼容。
- 构建镜像需要访问 registry/软件源；沙箱运行阶段有意保持断网。
- Cube SDK 的 create、snapshot 等控制面操作目前不暴露独立的单次调用 timeout；
  命令执行使用自己的有限 timeout。
- 按设计，原生 TCP 客户端无法连接 5432；请使用 SDK 命令通道和本地 Unix Socket。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| Docker 构建在 Docker Hub/GHCR 镜像层长时间卡住 | Docker daemon 不会自动继承 shell 代理，或宿主 DNS/registry 直连路由异常 | 配置 Docker daemon 代理或 registry mirror；也可以预拉取两个基础镜像并打成本地标签。若 `apt` 同样需要宿主代理，请传入小写 `http_proxy`/`https_proxy` build args，并选择合适的 build network mode |
| Docker health 一直为 `starting` 或变为 `unhealthy` | PostgreSQL 未初始化、socket 权限错误或 envd 未启动 | 执行 `docker logs cubesandbox-postgresql-stateful`；确认以 `postgres` 运行 `pg_isready` 可以成功 |
| 模板构建的探针超时 | 端口/路径不匹配，或 PostgreSQL 未能在探针窗口内就绪 | 公开并探测 49983 的 `/health`；检查镜像 job 和 Cubelet 日志 |
| 模板为 `READY`，但 distribution 未完成 | 一个或多个目标节点还没有收到 artifact | 等待后重新运行 `tpl info`；检查对应 Cubelet 的镜像/存储日志 |
| 手动 envd 请求被拒绝 | 请求没有携带该沙箱的流量令牌 | 复用 create 返回的 `Sandbox` 对象；手动 HTTP 请求需携带其流量令牌 header |
| SDK 无法解析 `*.cube.app` | 宿主机无法解析沙箱通配域名 | 设置 `CUBE_PROXY_NODE_IP`，必要时同时设置 `CUBE_PROXY_PORT_HTTP` |
| 已设置 `HTTP_PROXY`/`HTTPS_PROXY` 时，本地 CubeAPI 请求返回 HTTP 502 | 指向回环地址的控制面请求被错误送入宿主代理 | 把 `127.0.0.1`、`localhost` 和 CubeProxy 节点 IP 同时加入 `NO_PROXY` 与 `no_proxy` |
| `psql: Peer authentication failed` | 命令使用了错误的系统用户 | 使用示例 helper，或传入 `user="postgres"` |
| `pg_isready -h 127.0.0.1` 失败 | 无需处理，这是预期的安全配置 | 改用 `-h /var/run/postgresql` |
| `No space left on device` | 4-GiB 可写层已满 | 删除测试数据/snapshot，或用更大的可写层重建模板 |
| 脚本未打印最终 `OK:` 就退出 | 断言或平台调用失败 | 查看输出的命令 stdout/stderr，再列出 sandbox 和 snapshot 确认清理结果 |

## 验收清单

请在实际评审该贡献的环境中执行以下检查；本文档不声称某个外部集群已经通过这些检查。

```bash
python3 -m compileall examples/postgresql-stateful-sandbox
ruff check examples/postgresql-stateful-sandbox
shellcheck examples/postgresql-stateful-sandbox/*.sh
git diff --check

npm --prefix docs ci
npm --prefix docs run docs:build
```

此外，还必须完成第 1 步的本地镜像检查、让模板达到 `READY` 且全部节点分发完成、运行三个
Python 脚本，并比较运行前后的 sandbox/snapshot 列表，确认没有资源泄漏。

## 参考

- [从 OCI 镜像创建模板](../../docs/zh/guide/tutorials/template-from-image.md)
- [连接已有 Cube 集群](../../docs/zh/guide/connect-existing-cluster.md)
- [快照、回滚与克隆](../../docs/zh/guide/snapshot-rollback-clone.md)
- [网络策略](../../docs/zh/guide/network-policy.md)
- [限制公网访问](../../docs/zh/guide/restrict-public-access.md)
- [PostgreSQL 文件系统备份一致性](https://www.postgresql.org/docs/16/backup-file.html)
- [PostgreSQL `CHECKPOINT`](https://www.postgresql.org/docs/16/sql-checkpoint.html)
