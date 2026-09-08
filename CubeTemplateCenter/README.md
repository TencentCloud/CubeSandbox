# CubeTemplateCenter

模板中心的独立进程，负责构建模板：拉镜像、在 envd 沙箱里跑构建、生成 rootfs ext4、算指纹，再把结果回报给 CubeMaster。CubeMaster 负责剩下的：任务落库、对外 API、跨节点分发。

逻辑代码在 `CubeMaster/pkg/templatecenter`，TC 只是把它跑成独立进程。

## 和 CubeMaster 的分工

| 职责 | CubeMaster | TC |
|---|---|---|
| 模板 API（`/cube/template*`） | 提供 | 不提供 |
| 任务落库 / 状态机 | 负责 | 不负责 |
| 构建（拉镜像、建 ext4、指纹） | 不做 | 负责 |
| 产物发给 Cubelet | 从共享磁盘读 | 只写盘 |
| 跨节点分发 / redo | 负责 | 不负责 |

产物默认不走网络：两者挂同一块磁盘（`/data/CubeMaster/storage`），TC 写、CubeMaster 读，所以默认必须同机、单副本。配置 S3/MinIO（`controlPlane.artifactStore.s3Backed=true`）后持久副本在对象存储里，本地盘只是构建临时区，此时 TC 和 CubeMaster 都可以多副本（见下文"多副本"）。

## 配置

没有开关：CubeMaster 已经不能本地构建模板，所有 `template-from-image` 都会转发给 TC，TC 不可达就直接失败。只需要两个地址：

| 项 | 在哪 | 值 |
|---|---|---|
| CubeMaster 找 TC | 环境变量 | `CUBE_TEMPLATE_CENTER_ADDR`，如 `http://127.0.0.1:8090` |
| TC 回报 CubeMaster | 环境变量 | `CUBE_MASTER_ADDR`，如 `http://127.0.0.1:8089` |

地址随部署变，所以走环境变量（也可以在 CubeMaster `conf.yaml` 用 `common.template_center_addr` 持久化配置，环境变量优先）。

## 启动

没有 `-conf` 参数，靠环境变量找配置：

```bash
export CUBE_TEMPLATE_CENTER_CONFIG_PATH=/path/to/conf.yaml
export CUBE_MASTER_ADDR=http://127.0.0.1:8089
./templatecenter
```

默认监听 `:8090`（CubeMaster 是 `:8089`）。监听地址和端口在 conf.yaml 的 `common.http_bind`、`common.http_port`。

## 部署

**Kubernetes（推荐）**，Helm 直接装（TC 是管控面默认组件，`controlPlane.enabled=true` 时自动部署，没有独立开关）：

```bash
helm upgrade --install cube deploy/kubernetes/chart -n cube-system
```

conf、双向地址、PVC、同节点亲和都自动配好。

**裸机 / one-click**：`cube-sandbox-cube-templatecenter.service` 属于默认管控面组件（control target 的 `Wants=` 已包含，install.sh 会显式 enable），模板构建开箱即用。默认地址 `http://127.0.0.1:8090` 已经由 `cubemaster-start.sh` 导出，跨机部署才需要在 `.one-click.env` 覆盖 `CUBE_TEMPLATE_CENTER_ADDR`。

**多副本**：推荐 `controlPlane.artifactStore.s3Backed=true`（持久副本在 S3/MinIO，本地盘只做临时区）：副本之间通过数据库会话锁（`GET_LOCK`，按构建指纹）协调同规格的去重构建，chart 的 validate 会校验前置条件（S3 凭据到达 master 和 TC Pod、本地盘为 per-Pod scratch 等）。不用 S3 时多副本也允许但属于**降级模式**（安装时会打印告警）：产物在节点本地盘，下载可能落到没构建过该产物的 master 副本上，TC 副本间也无法去重构建。注意：默认的 ReadWriteOnce 产物 PVC 无法多节点挂载，多副本 master 挂着它会被 validate 直接拒绝（第二个副本永远 Pending）——不用 S3 又要多副本时，请 `controlPlane.master.persistence.enabled=false`（每 Pod 临时盘）或用 ReadWriteMany 共享卷（CFS/NFS，TC 默认挂 master 的同一张 claim）。

## API

主要是 CubeMaster 内部调用：

| 方法 | 路径 | 用途 |
|---|---|---|
| POST | `/tc/api/v1/build` | 提交构建任务 |
| GET | `/health` | 探针 |
| GET | `/metrics` | Prometheus 指标 |

构建完成后 TC 主动回报：`POST $CUBE_MASTER_ADDR/internal/template/jobs/:job_id/status`。

## 目录

```
pkg/tcconfig/     环境变量读取
pkg/build/        构建执行 + 状态回报
pkg/reconcile/    任务对账
pkg/httpservice/  gin server（只注册模板路由）
```

---

[English](README_EN.md)
