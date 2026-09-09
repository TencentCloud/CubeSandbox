# CubeTemplateCenter

模板中心的独立进程，负责构建模板：拉镜像、在 envd 沙箱里跑构建、生成 rootfs ext4、算指纹，再把结果回报给 CubeMaster。CubeMaster 负责剩下的：任务落库、对外 API、跨节点分发。

路由层复用 `CubeMaster/pkg/service/httpservice`（`RegisterTemplateRoutes`）；构建与存储逻辑在 TC 自己的 `pkg/build`、`pkg/image`、`pkg/s3store`。

## 和 CubeMaster 的分工

TC 负责构建（拉镜像、ext4、指纹、S3 上传）和产物数据（本地文件 + S3 对象）。
CubeMaster 负责模板 API、任务状态、跨节点分发和删除编排。
compat 矩阵和下载路由由 CubeMaster 反代到 TC（下载走 S3 则 302）。

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

## 使用场景（拓扑矩阵）

| 场景 | TC 副本 | 产物存储 | 是否支持 |
|---|---|---|---|
| 裸机 / one-click（同机） | 1 | 与 CubeMaster 共享宿主机本地盘 | ✅ |
| one-click 多节点（1 控制面 + N 计算节点） | 控制面 1 个 | 控制面宿主机本地盘 | ✅ — 计算节点上设 `ONE_CLICK_DEPLOY_ROLE=compute` |
| K8s 单副本 | 1 | 本地盘（PVC 推荐；emptyDir 重启丢产物） | ✅ — **保持 1 副本**；强行开多副本 + 本地盘会坏（每个副本只存自己的构建，任何 LB 都会把下载路由到非构建副本 → 404） |
| K8s 多副本 TC | ≥2 | **必须** `s3Backed=true` **或** ReadWriteMany | ✅ |
| K8s 多副本 master | TC 单副本 | 本地盘 | ✅ — 下载走反代到唯一 TC |

**不支持**——每个 TC 副本必须拿到同一份字节；把外部流量随机打到"任意
副本"的 LB 仅在产物存储共享时才有意义。`helm install` 在编译期直接
拒绝以下组合：

- **K8s + 内部 CLB + 多副本 TC + 本地盘。** CLB 把外部请求 LB 到多个
  TC 副本，但每个副本的构建只在自己盘上 → 打到没构建过的副本就 404。
  修复：`s3Backed=true`、ReadWriteMany、或 `templateCenter.replicas=1`。
- **K8s + 多副本 master + 默认 ReadWriteOnce 产物 PVC。** 第二副本永远
  Pending。修复：`master.persistence.enabled=false` 或 ReadWriteMany。
- **one-click 多控制面**（两台主机各自跑完整安装、各自有自己的存储）。
  安装器只建模"控制面 + 计算节点"，不建模"两个控制面"。要先扩控制面，
  请迁 Helm chart + `s3Backed=true`。

## API

内部端点（CubeMaster 调用）：

| 方法 | 路径 | 用途 |
|---|---|---|
| POST | `/tc/api/v1/build` | 提交构建任务 |
| POST | `/tc/api/v1/artifact/delete` | 删除产物数据（本地文件 / S3 对象） |

经 CubeMaster 反代的公共路由（TC 实际服务）：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET / HEAD | `/cube/template/artifact/download` | 产物下载（本地盘直接服务；S3 产物 302 到 presigned URL） |
| GET / POST | `/cube/template/compat` | 模板兼容矩阵读写 |

构建状态轮询（`/cube/template/build/:build_id/status`、`/cube/template/from-image?job_id=`）留在 Master 本地服务（读的是 Master 落库的任务行）；TC 也注册这两个路由，供绕开 Master 直连 TC 的场景使用。

探针与指标：`GET /health`、`GET /metrics`（Prometheus）。

构建完成后 TC 主动回报：`POST $CUBE_MASTER_ADDR/internal/template/jobs/:job_id/status`。

## 目录

```
pkg/tcconfig/     环境变量读取
pkg/build/        构建执行 + 状态回报 + 产物删除
pkg/image/        镜像拉取与 ext4 生成
pkg/s3store/      S3/MinIO 产物存取
pkg/lock/         跨副本 DB 会话锁（构建去重 / reconciler 互斥）
pkg/cube_egress_ca/ 模板内烘焙 CubeEgress 根 CA
pkg/reconcile/    任务对账
pkg/api/          内部端点（/tc/api/v1/*）
pkg/httpservice/  gin server（健康检查 / 指标 + 注册反代路由）
```

---

[English](README.md)
