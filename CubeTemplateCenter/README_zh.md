# CubeTemplateCenter

CubeTemplateCenter（TC）是 CubeSandbox 的**模板构建与 artifact 存储数据面**。它负责拉取 OCI 镜像、在构建沙箱里生成 rootfs ext4、上传 / 保管 artifact，并把构建结果回报给 CubeMaster。CubeMaster 继续负责对外 API、job 落库、模板元数据、跨节点分发和删除编排。

路由层复用 `CubeMaster/pkg/service/httpservice`（`RegisterTemplateRoutes`）；构建与存储逻辑在 TC 自己的 `pkg/build`、`pkg/image`、`pkg/s3store`。

## 和 CubeMaster 的分工

| 主题 | CubeTemplateCenter | CubeMaster |
| --- | --- | --- |
| `from-image` 构建 | 拉镜像、解包、生成 ext4、写 artifact、回报状态 | 创建 / 复用 job、对外暴露 API、转发构建请求 |
| `tpl merge` | 接收本地 ext4 上传到 TC 自己的 artifact store | 发起 `/cube/template/migrate`、落 migrate job、更新 artifact 行 |
| 模板元数据 | 不拥有模板 definition / alias / replica | 拥有 definition / alias / replica / compat / job 行 |
| 下载 | 实际提供下载字节流（或 302 到 S3） | 暴露公共下载路由并反代到 TC |
| 删除 | 物理删除本地文件 / S3 对象 | 编排引用计数、placement 清理、最终通知 TC 删除 |

一句话概括：**TC 管字节，CubeMaster 管状态与编排。**

## 典型操作流

### 1. 从镜像创建模板

用户通过 `cubemastercli` 调 CubeMaster；CubeMaster 落 job 并把构建提交给 TC：

```bash
cubemastercli --address <cubemaster-host> --port 8089 tpl create-from-image \
  --image ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 10Gi \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

构建期间，轮询读取的是 CubeMaster 落库的 job 行：

```bash
cubemastercli --address <cubemaster-host> --port 8089 tpl watch --job-id <job-id>
cubemastercli --address <cubemaster-host> --port 8089 tpl status --job-id <job-id>
```

### 2. 把历史本地产物迁到 TC 存储（`tpl merge`）

`tpl merge` 的语义是：**把一个已经 `READY` 的模板 rootfs artifact，从 CubeMaster 本地磁盘迁到 TC 管理的 artifact store（S3 或 TC 本地 store）**。CLI 命令名叫 `merge`，但对应的 API 路径是 `/cube/template/migrate`。

```bash
cubemastercli --address <cubemaster-host> --port 8089 tpl merge <template-id>
```

默认会**阻塞等待 migrate job 结束**；如果只想提交任务就返回，使用：

```bash
cubemastercli --address <cubemaster-host> --port 8089 tpl merge <template-id> --detach
```

适用场景：

- **历史模板 / 旧部署**：artifact 还只在 CubeMaster 本地盘上。
- **开启了 `s3Backed=true` 之后**：希望把旧的本地 ext4 收敛进 S3。
- **清理残留本地副本**：当 artifact 已经是 S3-backed 时，再次执行 `tpl merge` 会做幂等检查，并尽量清理遗留的本地 ext4。

对于**存量镜像对应的历史模板**，文档口径应统一为：**`tpl merge` 解决历史 artifact 的存储收敛问题，`tpl redo` 解决节点侧重新分发 / 必要时重建问题。**

典型场景是：模板最初的 artifact 仍保存在 CubeMaster 本地盘，后续集群开启了 `s3Backed=true`，需要将这批历史 artifact 从本地盘迁移到 **S3 托管存储**。如果同一次运维还需要让模板重新覆盖目标节点，则在 `tpl merge` 完成后继续执行 `tpl redo`。

> **高亮提醒**
> 在默认共盘 / 共享 PVC 部署里，不执行 `tpl merge` 通常**不会立刻影响现有模板下载**；真正的问题是这些历史 artifact 仍未完成从**本地盘到 S3 托管存储**的收敛。
>
> - **存储侧**：开启 `s3Backed=true` 后，旧模板不会自动补做迁移。
> - **恢复侧**：如果本地 ext4 已经丢失，再补跑 `tpl merge` **也修不回来**；因为已经没有可上传的文件，这时只能对可重建的 `from-image` 模板通过 `tpl redo` 回退到重建流程。

如果你的场景**既要把旧文件迁进统一存储，又要重新覆盖节点**，可以按下面的顺序执行：

```bash
cubemastercli --address <cubemaster-host> --port 8089 tpl merge <template-id>
cubemastercli --address <cubemaster-host> --port 8089 tpl redo --template-id <template-id>
```

注意：`tpl merge` **不是** `tpl render` / `merged_request` 里的“请求合并”；它只处理 rootfs artifact 的**存储迁移**。

### 3. 用独立验证工具做端到端验证

仓库里提供了一个**完全独立**的 Go 验证工具：`CubeMaster/cmd/templateverify`。它不依赖 `cubemastercli` 命令体系，会自己发 HTTP、轮询 job，并可选做 MySQL / 下载校验。

```bash
cd CubeMaster
go run ./cmd/templateverify \
  --master http://127.0.0.1:8089 \
  --image ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 10Gi \
  --skip-db
```

如果需要把 `create-from-image -> merge -> artifact 下载` 整条链路一次性打通，优先使用这个工具。

## 启动

没有 `-conf` 参数，靠环境变量找配置：

```bash
export CUBE_TEMPLATE_CENTER_CONFIG_PATH=/path/to/conf.yaml
export CUBE_MASTER_ADDR=http://127.0.0.1:8089
./templatecenter
```

默认监听 `:8090`（CubeMaster 默认是 `:8089`）。监听地址和端口来自 `conf.yaml` 的 `common.http_bind`、`common.http_port`。

## 部署

### Kubernetes（推荐）

Helm 直接安装即可。TC 是默认管控面组件；`controlPlane.enabled=true` 时会自动部署：

```bash
helm upgrade --install cube deploy/kubernetes/chart -n cube-system
```

chart 会自动接好：

- `CUBE_TEMPLATE_CENTER_ADDR` / `CUBE_MASTER_ADDR`
- artifact PVC / 存储类
- 同节点亲和（本地盘模式）
- 健康检查与 Service

### 裸机 / one-click

`cube-sandbox-cube-templatecenter.service` 属于默认 control-plane 组件。安装完成后模板构建默认可用：

- 默认 TC 地址：`http://127.0.0.1:8090`
- 默认由 `cubemaster-start.sh` 导出到 `CUBE_TEMPLATE_CENTER_ADDR`
- 只有跨机拆分部署时，才需要在 `.one-click.env` 覆盖 `CUBE_TEMPLATE_CENTER_ADDR`

## 使用场景（拓扑矩阵）

| 场景 | TC 副本 | 产物存储 | 是否支持 |
| --- | --- | --- | --- |
| 裸机 / one-click（同机） | 1 | 与 CubeMaster 共享宿主机目录（`/data/CubeMaster/storage`） | ✅ |
| K8s 默认（TC 单副本） | 1 | 复用 master 的 artifact PVC，并使用相同容器内路径（`/data/CubeMaster/storage`） | ✅ |
| one-click 多节点（1 控制面 + N 计算节点） | 控制面 1 个 | 控制面宿主机本地盘 | ✅ — 计算节点上设 `ONE_CLICK_DEPLOY_ROLE=compute` |
| K8s 单副本 | 1 | 本地盘（PVC 推荐；`emptyDir` 重启丢产物） | ✅ — **保持 1 副本**；强行多副本 + 本地盘会把下载流量打到没构建过的副本 → 404 |
| K8s 多副本 TC | ≥ 2 | **必须** `s3Backed=true` **或** ReadWriteMany | ✅ |
| K8s 多副本 master | TC 单副本或多副本 | 共享存储 / S3 | ✅ |

**不支持**：把外部流量随机打到多个 TC 副本，但这些副本又看不到同一份 artifact 字节。

chart 会拒绝以下组合：

- **K8s + 内部 CLB + 多副本 TC + 本地盘**：CLB 会把下载打到没持有该 artifact 的副本。
- **K8s + 多副本 master + 默认 ReadWriteOnce artifact PVC**：第二个 master 永远 `Pending`。
- **one-click 多控制面**（两台机器各自完整安装、各自有独立存储）：安装器只建模“控制面 + 计算节点”，不建模“双控制面共享模板存储”。

## API

### TC 内部端点（CubeMaster 调用）

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/tc/api/v1/build` | 提交 `from-image` 构建任务 |
| POST | `/tc/api/v1/artifact/upload` | 接收 CubeMaster 通过 `tpl merge` 上传的本地 ext4，写入 TC artifact store |
| POST | `/tc/api/v1/artifact/delete` | 删除 artifact 物理数据（本地文件 / S3 对象） |

### 通过 CubeMaster 暴露的用户路径

| 方法 | 路径 | 实际服务方 | 用途 |
| --- | --- | --- | --- |
| POST | `/cube/template/from-image` | CubeMaster（本地） | 创建 / 复用模板构建 job，并转发给 TC |
| GET | `/cube/template/from-image?job_id=...` | CubeMaster（本地） | 查询 `from-image` job 状态 |
| POST | `/cube/template/migrate` | CubeMaster（本地） | 提交 artifact migrate job（CLI 命令：`tpl merge`） |
| GET | `/cube/template/migrate?job_id=...` | CubeMaster（本地） | 查询 migrate job 状态 |
| GET / HEAD | `/cube/template/artifact/download` | TC（经 CubeMaster 反代） | 下载 artifact；S3-backed 时 302 到 presigned URL |
| GET / POST | `/cube/template/compat` | TC（经 CubeMaster 反代） | 读写模板 compat 矩阵 |

另外，`/cube/template/build/:build_id/status` 仍由 CubeMaster 本地服务，用于 `tpl commit` 的 build-status / build-watch。

### 探针与回调

- 健康检查：`GET /health`
- 指标：`GET /metrics`
- 构建完成后 TC 主动回调 CubeMaster：`POST $CUBE_MASTER_ADDR/internal/template/jobs/:job_id/status`

## 目录

```text
pkg/tcconfig/        环境变量读取
pkg/build/           构建执行、状态回报、artifact 删除
pkg/image/           镜像拉取与 ext4 生成
pkg/s3store/         S3 / MinIO artifact 存取
pkg/lock/            跨副本 DB 会话锁（构建去重 / reconciler 互斥）
pkg/cube_egress_ca/  模板内烘焙 CubeEgress 根 CA
pkg/reconcile/       job 对账
pkg/api/             内部端点（/tc/api/v1/*）
pkg/httpservice/     gin server（健康检查 / 指标 + 注册反代路由）
```

---

[English](README.md)
