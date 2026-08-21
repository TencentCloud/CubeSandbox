# CubeTemplateCenter

模板中心的独立进程，负责构建模板：拉镜像、在 envd 沙箱里跑构建、生成 rootfs ext4、算指纹，再把结果回报给 CubeMaster。CubeMaster 负责剩下的：任务落库、对外 API、跨节点分发。

逻辑代码在 `CubeMaster/pkg/templatecenter`，TC 只是把它跑成独立进程。

## 和 CubeMaster 的分工

| 职责 | CubeMaster | TC |
|---|---|---|
| 模板 API（`/cube/template*`） | 提供 | 不提供 |
| 任务落库 / 状态机 | 负责 | 不负责 |
| 构建（拉镜像、建 ext4、指纹） | 默认本地做 | 启用后接管 |
| 产物发给 Cubelet | 从共享磁盘读 | 只写盘 |
| 跨节点分发 / redo | 负责 | 不负责 |

产物不走网络：两者挂同一块磁盘（`/data/CubeMaster/storage`），TC 写、CubeMaster 读。所以必须同机、单副本。

## 配置

一个开关，两个地址：

| 项 | 在哪 | 值 |
|---|---|---|
| 是否用 TC 构建 | CubeMaster `conf.yaml` | `templatecenter_enabled: true/false`，默认 false |
| CubeMaster 找 TC | 环境变量 | `CUBE_TEMPLATE_CENTER_ADDR`，如 `http://127.0.0.1:8090` |
| TC 回报 CubeMaster | 环境变量 | `CUBE_MASTER_ADDR`，如 `http://127.0.0.1:8089` |

地址随部署变，所以走环境变量，不进 conf。开关是 false 时两个地址都不读。

## 启动

没有 `-conf` 参数，靠环境变量找配置：

```bash
export CUBE_TEMPLATE_CENTER_CONFIG_PATH=/path/to/conf.yaml
export CUBE_MASTER_ADDR=http://127.0.0.1:8089
./templatecenter
```

默认监听 `:8090`（CubeMaster 是 `:8089`）。监听地址和端口在 conf.yaml 的 `common.http_bind`、`common.http_port`。

## 部署

**Kubernetes（推荐）**，Helm 一个参数：

```bash
helm upgrade --install cube deploy/kubernetes/chart \
  -n cube-system --set controlPlane.templateCenter.enabled=true
```

conf、双向地址、PVC、同节点亲和都自动配好。

**裸机 / one-click**：`cube-sandbox-cubetemplatecenter.service` 已装好但默认不启动。启用：CubeMaster `conf.yaml` 设 `templatecenter_enabled: true`，`.one-click.env` 设 `CUBE_TEMPLATE_CENTER_ADDR`，重启两个服务。

**为什么只能单副本**：产物在节点本地盘，没有跨节点共享。起第二个副本，读不到第一个的文件，也接不了它的构建，还会抢同一个目录。要高可用，扩 CubeMaster。

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
