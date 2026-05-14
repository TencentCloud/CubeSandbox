# 从 OCI 镜像制作模板

本文介绍如何使用 `cubemastercli` 命令行工具，从标准 OCI 容器镜像出发，完成模板的创建、进度监控和删除操作。

## 概述

**模板（Template）** 是一份预构建的不可变 rootfs 快照，沙箱运行时用它来冷启动（或热启动）新的沙箱实例。从 OCI 镜像制作模板是一个在集群上**异步**执行的三阶段流水线：

```
OCI 镜像  ──拉取──►  ext4 rootfs  ──启动──►  快照  ──注册──►  模板 READY
```

模板进入 `READY` 状态后，即可通过其 `template_id` 创建沙箱实例。

---

## 前置条件

- 已安装 `cubemastercli` 并加入 `$PATH`
- 设置环境变量 `CUBEMASTER_ADDR`，或在每条命令中加 `--server <host>`
- OCI 镜像须可被 CubeMaster 节点访问（公开仓库或已配置认证的私有仓库）

### ⚠️ 镜像必须提供 HTTP 服务

Cube 平台在制作模板时，会启动容器并**通过 HTTP 探测**容器是否已就绪。因此：

1. 你的容器镜像**必须**在某个固定端口上启动一个 HTTP 服务器。
2. 创建模板时**必须**指定以下参数：
   - `--expose-port <port>` — 声明 HTTP 服务监听的端口
   - `--probe <port>` — 告诉 Cube 要探测哪个端口
   - `--probe-path <path>` — Cube 将 `GET` 的 HTTP 路径（如 `/` 或 `/health`）
3. 你的容器入口程序应在**应用完全准备好对外提供服务之后**，再启动 HTTP 服务——Cube 在探针返回 HTTP 2xx 时即将模板标记为就绪，由此模板创建的沙箱会立刻开始接收请求。

如果容器未暴露 HTTP 服务，或探针参数配置错误，模板制作将因超时而失败。

---

## 第一步 — 创建模板

使用 `tpl create-from-image` 子命令发起构建任务：

```bash
cubemastercli tpl create-from-image \
  --image     cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`。

命令成功后立即返回 `job_id` 和自动生成的 `template_id` 并退出，构建任务在集群后台继续执行：

```
job_id:      0042cd3a-c1d6-45fd-8757-2595ba0027e8
template_id: tpl-4ff5adc5eea44c14b1c8dbb3
attempt_no:  1
artifact_id:
status:      PENDING
phase:       PULLING
progress:    0%
```

#### 示例 — 多端口 + 自定义探针路径 + 环境变量

```bash
cubemastercli tpl create-from-image \
  --image     cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe      49999 \
  --probe-path /healthz \
  --env        MY_ENV=production
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

---

## 第二步 — 监控进度

有两种方式跟踪构建任务。

### Watch（阻塞，推荐）

`tpl watch` 循环轮询任务，直到任务到达终态（`READY` 或 `FAILED`）才退出：

```bash
cubemastercli tpl watch --job-id <job_id>
```

任务完成时的示例输出：

```
job_id:                    2e71b561-153e-4c08-ac37-5270d94f5f15
template_id:               tpl-748094d2f2374b0a8a37e6ec
attempt_no:                1
artifact_id:               rfs-1e8e07c90e9bb8eff94ecde2
status:                    READY
phase:                     READY
progress:                  100%
distribution:              1/1 ready, 0 failed
template_spec_fingerprint: 1e8e07c90e9bb8eff94ecde20396002c411f6b812612a2a05086b85fe245b858
artifact_status:           READY
artifact_sha256:           5d413bc735062d49d36ef9c0e62cd0c3a915853be5ec0c7fba90e13d9fd33f79
template_status:           READY
```

主要输出字段说明：

| 字段 | 说明 |
|------|------|
| `status` / `template_status` | 任务和模板的整体状态。`READY` 表示模板可用。 |
| `phase` | 当前流水线阶段：`PULLING`（拉取镜像）→ `BUILDING`（构建 rootfs）→ `DISTRIBUTING`（分发到节点）→ `READY`。 |
| `progress` | 当前阶段完成百分比。 |
| `distribution` | `N/M ready` — 已收到 artifact 的集群节点数。 |
| `artifact_id` | 构建出的 rootfs artifact 的稳定 ID。 |
| `artifact_sha256` | rootfs artifact 的 SHA-256 摘要，用于完整性校验。 |
| `template_spec_fingerprint` | 模板规格的确定性指纹（镜像 + 构建参数），相同输入始终产生相同指纹。 |

### Status（单次查询）

只需查看一次当前状态而不阻塞时使用：

```bash
cubemastercli tpl status --job-id <job_id>
```

---

## 第三步 — 使用模板

`template_status: READY` 后，通过 `template_id` 使用 E2B SDK 创建沙箱：

```bash
export CUBE_TEMPLATE_ID=tpl-748094d2f2374b0a8a37e6ec
python CubeAPI/examples/create.py
```

---

## 查询模板

### 列出所有模板

```bash
cubemastercli tpl list
```

输出示例：

```
TEMPLATE_ID                  INSTANCE_TYPE   STATUS   CREATED_AT             IMAGE_INFO
tpl-748094d2f2374b0a8a37e6ec cubebox         READY    2026-04-02T08:10:30Z   docker.io/library/nginx:latest@sha256:abcd...
tpl-4ff5adc5eea44c14b1c8dbb3 cubebox         READY    2026-04-01T17:42:11Z   docker.io/library/python:3.11
```

`CREATED_AT` 使用 UTC RFC3339 格式输出。`IMAGE_INFO` 会优先展示镜像引用 +
digest（`image@sha256:...`）；当 digest 不可用时降级为仅展示镜像引用。

如果需要同时查看 `VERSION` 和 `LAST_ERROR`，使用宽格式输出：

```bash
cubemastercli tpl list -o wide
```

加 `--json` 输出完整 JSON，便于脚本处理：

```bash
cubemastercli tpl list --json | jq '.data[].template_id'
```

### 查看单个模板详情

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec
```

需要机器可读输出时，加上 `--json`：

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec --json
```

如果想查看模板里保存的创建请求体，可再加 `--include-request`：

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec --json --include-request
```

如果想预览创建沙箱时最终生效的请求，可使用：

```bash
cubemastercli tpl render --template-id tpl-748094d2f2374b0a8a37e6ec --json
```

如果你更关心“应该看什么、如何一步步预览最终请求”，可继续阅读[模板检查与请求预览](../template-inspection-and-preview.md)。

---

## 删除模板

```bash
cubemastercli tpl delete --template-id tpl-748094d2f2374b0a8a37e6ec
```

成功后输出：

```
template deleted: tpl-748094d2f2374b0a8a37e6ec
```

> ⚠️ 删除操作会同时移除模板元数据和所有节点上的 artifact 副本。已基于该模板运行的沙箱**不受影响**，但此后无法再用该模板创建新沙箱。

---

## 常见问题

| 现象 | 可能原因 | 处理方式 |
|------|----------|----------|
| `phase: PULLING` 长时间卡住 | 镜像拉取慢或集群节点无法访问镜像仓库 | 检查网络/防火墙；私有仓库需添加 `--registry-username` / `--registry-password` |
| `status: FAILED`（BUILDING 阶段） | 构建错误（磁盘满、Dockerfile 问题等） | 执行 `tpl status --job-id <id> --json` 查看 `last_error` 字段 |
| `distribution: 0/N ready`（状态已 READY） | artifact 分发仍在进行（短暂正常） | 等待后重新执行 `tpl info`；若长时间未恢复检查目标节点的 Cubelet 日志 |
| 沙箱启动后就绪探针一直失败 | 容器内服务未在预期端口/路径监听，或服务尚未完全就绪时 HTTP server 已提前启动 | 确认 HTTP server 在应用完全就绪后再启动；检查 `--probe-path` 是否正确 |
