# 沙箱资源指标

Cubelet 为运行中的 Cube 沙箱提供 CPU 和内存指标。一键安装默认启用该功能，指标端点为：

```text
http://<cubelet-node>:9998/v1/metrics/resource
```

该端点与 Cubelet 通用指标端点 `/v1/metrics` 相互独立。现有的 containerd cgroup monitor 仍保持启用，并可继续通过 `/v1/metrics` 导出通用 `container_*` 指标；本文介绍的 Cube 原生沙箱指标由独立链路采集，并通过 `/v1/metrics/resource` 导出。

Cubelet 在后台定期采集所选统计视角的资源数据，并将最新结果缓存在内存中。Prometheus 抓取时只读取缓存，不会在 HTTP 请求过程中同步访问所有沙箱。因此，抓取请求不会额外触发与沙箱数量成比例的运行时 RPC；但响应体大小和序列化开销仍会随时间序列数量增长。

::: warning 网络访问
Cubelet 的 HTTP 服务本身不提供鉴权或 TLS。

仅应在可信的管理网络中开放 `9998` 端口，或通过防火墙、安全组等方式限制访问来源。详见[网络加固](./network-hardening.md)。
:::

## 使用前提

- 资源指标仅面向运行中的 Cube 沙箱。沙箱暂停、终止或删除后，对应指标会停止导出。
- 当前版本面向单容器沙箱，主工作负载使用 `container_id == sandbox_id`。暂不提供多容器级别的资源拆分。
- `guest_workload` 要求沙箱内的 `cube-agent` 支持资源指标能力版本 `1`，并使用 cgroup v2 统一层级。
- `host_sandbox` 只依赖宿主机上的沙箱 cgroup，兼容范围更广，也是默认采集和导出的统计视角。

## 资源统计视角

资源指标提供两个彼此独立的统计视角：

| 统计视角         | 统计范围                                                     | 适用场景                                   |
| ---------------- | ------------------------------------------------------------ | ------------------------------------------ |
| `host_sandbox`   | 宿主机内核统计的沙箱 cgroup，包含 CubeShim、VMM 及其他宿主机侧资源占用。 | 宿主机侧沙箱资源记账和基础运维观测。       |
| `guest_workload` | 沙箱内核统计的工作负载容器 cgroup，不包含 `cube-agent` 等沙箱管理进程。 | 用户代码的 CPU、内存使用率和内存限制观测。 |
| `all`            | 同时导出以上两组指标。                                       | 同时分析工作负载使用量和运行时开销。       |

::: tip 如何选择

- 只需要节点侧基础观测时，使用默认的 `host_sandbox`。
- 需要判断沙箱内工作负载的资源使用率或内存压力时，使用 `guest_workload`。
- 需要对比工作负载与运行时开销时，使用 `all`。
:::

两组指标来自不同的资源记账范围，**不应直接相加为一个通用总量**。

`host_sandbox` 的内存值是宿主机 cgroup 的记账值，会受到共享快照页和写时复制（COW）的影响，并不等同于沙箱内看到的逻辑工作集或按比例分摊的物理内存。由于 VMM 开销、共享页和私有 COW 页等因素，`host_sandbox` 与 `guest_workload` 的内存值通常不会相等。

## 工作原理

所有采集都由宿主机上的 Cubelet 主动发起，沙箱不会主动向宿主机推送指标。

### `host_sandbox`

Cubelet 直接读取宿主机上的沙箱 cgroup。该视角不依赖模板中的 `cube-agent` 版本，也不涉及沙箱内的统计周期。

宿主机侧指标继续使用节点自身的 cgroup 层级，并保留项目现有的 cgroup v1 和 cgroup v2 兼容逻辑。

### `guest_workload`

Cubelet 通过 containerd `Task.Stats` 调用 CubeShim，再由 CubeShim 调用沙箱内的 `cube-agent StatsContainer`，读取工作负载容器 cgroup。

该视角依赖以下能力：

- 沙箱内启用 cgroup v2 统一层级；
- `cube-agent` 声明资源指标能力版本 `1`；
- CubeShim 能够通过 `Task.Stats` 返回完整的工作负载统计；
- Cubelet 能够根据沙箱生命周期维护统计周期和计数器基线。

CubeShim 会在启动沙箱时通过 `agent.unified_cgroup_hierarchy=true` 为 `cube-agent` 启用 cgroup v2 统一层级。

旧模板中的 `cube-agent` 可能不支持上述能力。这类沙箱不会导出 `guest_workload`，但仍可导出 `host_sandbox`。

## Cubelet 配置

通过一键安装部署后，Cubelet 配置文件位于：

```text
/usr/local/services/cubetoolbox/Cubelet/config/config.toml
```

资源指标插件的默认配置如下：

```toml
[plugins."io.cubelet.internal.v1.resource-metrics"]
  enabled = true
  collection_interval = "5s"
  request_timeout = "2s"
  max_concurrent_requests = 8
  stale_after = "15s"
  export_scopes = ["host_sandbox"]
```

### 配置项说明

| 配置项                    | 说明                                                         |
| ------------------------- | ------------------------------------------------------------ |
| `enabled`                 | 是否启动资源指标采集，默认启用。设置为 `false` 后，端点仍返回 HTTP 200，但不会导出沙箱资源指标。 |
| `collection_interval`     | Cubelet 更新内存缓存的目标间隔。该配置决定 `Task.Stats` RPC 和宿主机 cgroup 的读取频率，与 Prometheus 抓取间隔相互独立。 |
| `request_timeout`         | 单次 `Task.Stats` 请求或宿主机 cgroup 读取的超时时间。       |
| `max_concurrent_requests` | 单个采集器的最大并发请求数。`guest_workload` 和 `host_sandbox` 分别使用该上限，两者的并发上限相互独立。 |
| `stale_after`             | 最近一次成功样本超过该时长后，停止导出对应统计视角。该值不得小于 `collection_interval`，并应为调度和短暂采集失败预留余量。 |
| `export_scopes`           | 控制采集和导出的统计视角。可设置为 `["host_sandbox"]`、`["guest_workload"]` 或 `["all"]`，默认值为 `["host_sandbox"]`。未选中的采集器不会启动；`["all"]` 会启动两个采集器。 |

发生短暂采集失败时，Cubelet 会继续导出最近一次成功样本。只有样本年龄超过 `stale_after` 后，对应统计视角才会停止导出。

配置校验失败会导致 Cubelet 启动失败，例如：

- `stale_after` 小于 `collection_interval`；
- `export_scopes` 包含不支持的值。

修改配置后，重启 Cubelet：

```bash
sudo systemctl restart cube-sandbox-cubelet.service
```

## Prometheus 抓取配置

建议为沙箱资源指标配置独立的抓取任务：

```yaml
scrape_configs:
  - job_name: cubesandbox-resource
    scrape_interval: 30s
    scrape_timeout: 10s
    metrics_path: /v1/metrics/resource
    static_configs:
      - targets:
          - <compute-node-ip>:9998
```

将目标地址替换为各 Cubelet 节点在可信管理网络中的地址。

使用独立抓取任务后，可以单独调整资源指标的抓取间隔、超时时间和 `metric_relabel_configs`，而不会影响 Cubelet 通用指标。

资源指标端点最多同时处理两个抓取请求。超过该上限的请求会返回 HTTP 503，以避免多个大响应同时占用 Cubelet 的 CPU 和内存。正常的单个 Prometheus 抓取任务不会触发该限制；如果出现 503，应检查是否有多个 Prometheus 实例或手工请求同时抓取同一节点。

### 验证指标端点

先确认 Cubelet 正常运行，并且节点上至少有一个状态为 `Up` 的沙箱：

```bash
sudo systemctl is-active cube-sandbox-cubelet.service
/usr/local/services/cubetoolbox/Cubelet/bin/cubecli cubebox ls -a --no-trunc
```

等待至少一个采集周期后，在 Cubelet 节点上执行：

```bash
curl -fsS http://127.0.0.1:9998/v1/metrics/resource | \
  grep '^cubesandbox_'
```

默认配置会导出 `cubesandbox_host_sandbox_*` 指标。

需要导出 `guest_workload` 时：

1. 将 `export_scopes` 设置为 `["guest_workload"]` 或 `["all"]`；
2. 重启 Cubelet；
3. 确认沙箱内的 `cube-agent` 兼容资源指标能力版本 `1`。

端点返回 HTTP 200 但响应体为空不一定表示故障。以下情况都会产生空结果：

- 节点上没有运行中的沙箱；
- 插件已禁用；
- Cubelet 尚未完成首次采样。

## 基础指标族

Cubelet 仅导出 Counter 和 Gauge 类型的基础指标。CPU 使用核数以及 CPU、内存使用率等派生值由 Prometheus 查询计算。

CPU 和内存上限仅在 cgroup 配置了有限值时导出。未配置上限时，不会使用 `0` 或某个极大值代替。当前内存用量不受此限制，只要对应统计视角可用就会导出。

### `host_sandbox` 指标

`host_sandbox` 指标仅包含 `sandbox_id` 标签。

累计指标只包含当前沙箱占用宿主机 cgroup 期间的使用量，不包含可复用 cgroup 槽位中上一个沙箱的历史。

| 指标                                                   | 类型    | 单位与说明                                           |
| ------------------------------------------------------ | ------- | ---------------------------------------------------- |
| `cubesandbox_host_sandbox_cpu_usage_seconds_total`     | Counter | 沙箱在宿主机上累计使用的 CPU 秒数。                  |
| `cubesandbox_host_sandbox_cpu_user_seconds_total`      | Counter | 累计使用的用户态 CPU 秒数。                          |
| `cubesandbox_host_sandbox_cpu_system_seconds_total`    | Counter | 累计使用的内核态 CPU 秒数。                          |
| `cubesandbox_host_sandbox_cpu_throttled_seconds_total` | Counter | 累计受到 CPU 限流的秒数。                            |
| `cubesandbox_host_sandbox_cpu_periods_total`           | Counter | 累计 CPU 调度周期数。                                |
| `cubesandbox_host_sandbox_cpu_throttled_periods_total` | Counter | 累计发生限流的 CPU 调度周期数。                      |
| `cubesandbox_host_sandbox_cpu_limit_cores`             | Gauge   | 宿主机沙箱 cgroup 的有限 CPU 上限，单位为 CPU 核数。 |
| `cubesandbox_host_sandbox_memory_current_bytes`        | Gauge   | 当前计入宿主机沙箱 cgroup 的内存字节数。             |
| `cubesandbox_host_sandbox_memory_limit_bytes`          | Gauge   | 宿主机沙箱 cgroup 的有限内存上限，单位为字节。       |
| `cubesandbox_host_sandbox_memory_failures_total`       | Counter | 当前沙箱占用该 cgroup 期间发生的内存限制失败次数。   |

### `guest_workload` 指标

`guest_workload` 指标包含以下标签：

- `sandbox_id`
- `container_id`

累计指标以当前统计周期（metrics epoch）为窗口，不包含模板、克隆或回滚继承的历史。统计周期的生命周期语义见后文。

| 指标                                                         | 类型    | 单位与说明                                         |
| ------------------------------------------------------------ | ------- | -------------------------------------------------- |
| `cubesandbox_guest_workload_cpu_usage_seconds_total`         | Counter | 累计使用的 CPU 秒数。                              |
| `cubesandbox_guest_workload_cpu_user_seconds_total`          | Counter | 累计使用的用户态 CPU 秒数。                        |
| `cubesandbox_guest_workload_cpu_system_seconds_total`        | Counter | 累计使用的内核态 CPU 秒数。                        |
| `cubesandbox_guest_workload_cpu_throttled_seconds_total`     | Counter | 累计受到 CPU 限流的秒数。                          |
| `cubesandbox_guest_workload_cpu_periods_total`               | Counter | 累计 CPU 调度周期数。                              |
| `cubesandbox_guest_workload_cpu_throttled_periods_total`     | Counter | 累计发生限流的 CPU 调度周期数。                    |
| `cubesandbox_guest_workload_cpu_limit_cores`                 | Gauge   | 配置的有限 CPU 上限，单位为 CPU 核数。             |
| `cubesandbox_guest_workload_memory_current_bytes`            | Gauge   | 当前计入工作负载 cgroup 的内存字节数。             |
| `cubesandbox_guest_workload_memory_limit_bytes`              | Gauge   | 配置的有限内存上限，单位为字节。                   |
| `cubesandbox_guest_workload_memory_failures_total`           | Counter | 当前统计周期内发生的内存限制失败次数。             |
| `cubesandbox_guest_workload_metrics_epoch`                   | Gauge   | 当前统计周期的序号。                               |
| `cubesandbox_guest_workload_metrics_epoch_start_time_seconds` | Gauge   | 当前统计周期的开始时间，使用 Unix 时间戳秒数表示。 |

## PromQL 示例

### 宿主机侧 CPU 使用核数

以下查询返回沙箱在宿主机侧平均使用的 CPU 核数，其中包含 VMM 和 CubeShim 等运行时开销：

```promql
rate(cubesandbox_host_sandbox_cpu_usage_seconds_total[5m])
```

### 宿主机侧 CPU 使用率

以下查询以宿主机沙箱 cgroup 配置的有限 CPU 上限为分母计算使用率：

```promql
100 *
rate(cubesandbox_host_sandbox_cpu_usage_seconds_total[5m])
/
cubesandbox_host_sandbox_cpu_limit_cores
```

如果未配置有限 CPU 上限，仍可查询实际使用的 CPU 核数，但无法计算具有明确分母的 CPU 使用率。

### 宿主机侧内存记账值

```promql
cubesandbox_host_sandbox_memory_current_bytes
```

该值适合观察宿主机 cgroup 当前记到沙箱上的内存，不应解释为沙箱内的逻辑工作集。

共享快照页可能使该值低于 `guest_workload` 的当前内存值；VMM、CubeShim 和私有 COW 页等宿主机侧开销也可能使该值更高。

### 宿主机侧内存使用率

```promql
100 *
cubesandbox_host_sandbox_memory_current_bytes
/
cubesandbox_host_sandbox_memory_limit_bytes
```

当宿主机沙箱 cgroup 未设置有限内存上限时，Cubelet 不会导出 `cubesandbox_host_sandbox_memory_limit_bytes`，因此无法计算基于上限的内存使用率。

### 工作负载 CPU 使用核数

以下查询返回工作负载在指定时间窗口内平均使用的 CPU 核数：

```promql
rate(cubesandbox_guest_workload_cpu_usage_seconds_total[5m])
```

例如，查询结果为 `0.5`，表示该工作负载在查询窗口内平均使用了约半个 CPU 核。

### 工作负载 CPU 使用率

以下查询以工作负载配置的有限 CPU 上限为分母计算使用率：

```promql
100 *
rate(cubesandbox_guest_workload_cpu_usage_seconds_total[5m])
/
cubesandbox_guest_workload_cpu_limit_cores
```

如果工作负载未配置有限 CPU 上限，仍可查询实际使用的 CPU 核数，但无法计算具有明确分母的 CPU 使用率。

### 工作负载内存使用率

```promql
100 *
cubesandbox_guest_workload_memory_current_bytes
/
cubesandbox_guest_workload_memory_limit_bytes
```

当工作负载未设置有限内存上限时，Cubelet 不会导出 `cubesandbox_guest_workload_memory_limit_bytes`，因此无法计算基于上限的内存使用率。

### 内存限制失败次数

以下查询返回最近 5 分钟内发生的内存限制失败次数：

```promql
increase(cubesandbox_guest_workload_memory_failures_total[5m])
```

### 识别统计周期变化

以下查询返回最近 5 分钟内已有时间序列的统计周期变化次数，可用于标记回滚等统计窗口切换：

```promql
changes(cubesandbox_guest_workload_metrics_epoch[5m])
```

## 生命周期语义

本节主要说明 `guest_workload` 的统计周期，以及两种视角在暂停、回滚和删除等操作中的行为。

### 为什么需要统计周期

CPU 使用时间和内存限制失败次数来自 cgroup 累计计数器。制作模板或快照时，这些计数器会随沙箱状态一起保存。从模板或快照创建新沙箱时会继承已有计数；执行回滚时，原始计数器还可能退回到快照时的值。

如果直接按照 `sandbox_id` 导出原始计数：

- 模板制作阶段产生的 CPU 和内存限制失败历史会被计入新沙箱；
- 回滚后，同一指标可能出现无法区分原因的数值倒退。

为解决这些问题，Cubelet 会为每个新的 `guest_workload` 状态建立一个统计周期，并将首次成功采样作为基线：

```text
导出的累计值 = 当前原始值 - 当前统计周期基线
```

这样可以排除继承的历史，并将回滚后的数据表示为新的统计窗口。当前内存用量是瞬时值，不进行基线扣减。

### `guest_workload` 生命周期

| 生命周期事件                     | 指标行为                                                     |
| -------------------------------- | ------------------------------------------------------------ |
| 新建沙箱、从模板或快照创建、克隆、重建工作负载 | 创建新的统计周期，并使用首次成功采样排除继承的累计历史。 |
| 创建快照或提交模板               | 保持当前统计周期。                                           |
| 回滚                             | 回滚期间停止导出；完成后创建新的统计周期，累计指标重新从 `0` 开始。如果运行时恢复请求发出后失败，新统计周期会保持准备状态，`guest_workload` 将继续不可用，直到后续回滚成功，或删除并重建沙箱。 |
| 暂停                             | 保持当前统计周期，但停止导出指标，而不是导出 `0`。           |
| 恢复运行                         | 继续暂停前的统计周期。                                       |
| 删除                             | 删除缓存和对应指标序列。                                     |
| Cubelet 重启                     | 从持久化状态恢复统计周期和基线，不重新计算已有窗口。         |

如果生命周期元数据发生瞬时故障，导致运行中的沙箱暂时没有持久化 fresh 统计周期，`guest_workload` 采集器会先重新建立并持久化 pending 统计周期，再继续采集。恢复失败时仍保持不可用，并在后续采集周期重试。

Prometheus 不理解 Cubelet 的统计周期语义。只有累计值实际下降时，`rate()` 和 `increase()` 才会按照计数器重置处理。

需要可靠识别回滚或新统计窗口时，应查询：

- `cubesandbox_guest_workload_metrics_epoch`
- `cubesandbox_guest_workload_metrics_epoch_start_time_seconds`

当前版本不导出按统计周期精确计算的内存峰值。内存当前值仍表示采样时刻的实际点值。

### `host_sandbox` 生命周期

`host_sandbox` 不使用 `guest_workload` 的统计周期，而是按照宿主机 cgroup 的分配关系统计：

- 新沙箱被分配到可复用的宿主机 cgroup 池槽位时，Cubelet 会在挂载沙箱进程前读取并持久化本次分配的基线。
- 升级 Cubelet 时已经存在的沙箱可能没有该持久化字段；Cubelet 会在升级后的首次成功采样中建立兼容基线。
- Cubelet 会在挂载沙箱进程前重试瞬时的分配计数器读取失败。如果多次尝试仍失败，沙箱仍会正常创建，但该沙箱的 `host_sandbox` 会保持不可用，不会导出缺失初始使用量的累计窗口。修复持续存在的宿主机 cgroup 读取问题后，需要重新创建沙箱。
- 新沙箱不会继承同一槽位中上一个沙箱的 CPU 或内存限制失败历史。
- 创建快照和执行回滚不会重置基线，因为这两类操作不会更换宿主机上的沙箱进程，也不会改变对应的 cgroup 分配关系。
- 暂停时停止导出，恢复运行后继续此前的累计值。
- 删除沙箱后，移除对应的指标序列。

::: tip 采集与抓取调优
`collection_interval` 控制 `Task.Stats` RPC 和宿主机 cgroup 的读取频率，Prometheus 的 `scrape_interval` 控制 HTTP 抓取和样本写入频率。节点沙箱较多或 Prometheus 每几分钟才抓取一次时，可以适当增大 `collection_interval`，并同步增大 `stale_after`。

资源端点只保存最新样本，不保存历史；发生在两次 Prometheus 抓取之间的短时内存峰值不会被保留。CPU 速率查询窗口内应包含多个成功写入 Prometheus 的样本。

两个间隔都不会减少活跃时间序列数量。显式设置 `export_scopes = ["all"]` 时，单容器沙箱最多导出 22 条时间序列；如需减少序列数量，应调整 `export_scopes` 或使用 Prometheus `metric_relabel_configs`。
:::

## 升级旧模板和快照

快照模板会捕获沙箱内的进程和内存状态。仅升级节点上的 Cubelet、CubeShim 或沙箱虚拟机镜像文件，**不会替换旧模板内存快照中的 `cube-agent`**。

旧模板中的 `cube-agent` 可能不具备本版本要求的 cgroup v2 统计语义，也不会声明资源指标能力版本 `1`。这类沙箱不会导出 `guest_workload`，但仍可导出 `host_sandbox`。

### 镜像构建模板

对于通过镜像构建的模板，需要执行模板 `redo`。

`redo` 会使用节点当前的沙箱虚拟机镜像和 `cube-agent`，为同一个模板 ID 重新制作副本。任务完成后，新创建的沙箱即可导出 `guest_workload` 指标。

节点升级后，Dashboard 里标了「需重建」的模板要先点「重建模板」，再建沙箱。其它模板可以继续直接创建（会按模板记录的版本在节点本地查找，或从组件仓库下载）。只有当你需要当前节点上的 guest image / `cube-agent`、以便新沙箱导出 `guest_workload` 指标时，才需要对已可用的模板再点一次「重建模板」。

### 用户快照和已有沙箱

- 对于从运行中沙箱创建的用户快照，应先使用兼容的新模板创建沙箱，再重新创建快照。
- 已经运行或暂停的旧沙箱仍保留内存中的旧 `cube-agent`，需要删除并重新创建才能完成升级。

一键安装包会将经过审核的 `cube-agent` 写入沙箱虚拟机镜像。只要模板是 `READY` 且没有标「需重建」，升级后仍可直接创建沙箱。

运行时，CubeShim 还会校验 `StatsContainer` 返回的资源指标能力版本，避免将旧版本的 `guest_workload` 数据识别为有效指标。

## 故障排查

| 现象                                      | 常见原因与处理方式                                           |
| ----------------------------------------- | ------------------------------------------------------------ |
| 无法连接 `9998`                           | 确认 `cube-sandbox-cubelet.service` 正常运行，并检查监听地址、防火墙和安全组。 |
| HTTP 200，但没有任何 `cubesandbox_*` 指标 | 确认插件为 `enabled = true`，节点上存在状态为 `Up` 的沙箱，并等待至少一个 `collection_interval`。暂停的沙箱不会导出指标。 |
| 配置了 `guest_workload`，但没有对应指标   | 确认 `export_scopes` 为 `["guest_workload"]` 或 `["all"]`。如果配置正确，通常是沙箱内的 `cube-agent` 未声明资源指标能力版本 `1`；镜像构建模板可执行 template `redo`。如果模板兼容但问题仍然存在，检查 Cubelet 业务日志中的 `Task.Stats` 错误。 |
| 原有指标突然消失                          | 沙箱可能已暂停或删除；也可能是连续采集失败后，样本年龄超过了 `stale_after`。 |
| 新沙箱没有 `host_sandbox` 指标            | Cubelet 可能在创建阶段已耗尽宿主机 cgroup 分配基线的读取尝试。检查 Cubelet 日志中的 `capture host metrics baseline`，修复持续存在的宿主机 cgroup 读取问题后重新创建沙箱。 |
| 抓取返回 HTTP 503                         | 同一节点已有两个资源指标抓取正在处理。检查是否存在重复抓取任务或并发手工请求。 |
| CPU 或内存上限指标缺失                    | 对应 cgroup 未配置有限上限，这是预期行为；CPU 累计使用量和当前内存用量仍可正常导出。 |
| 指标更新频率不符合预期                    | `collection_interval` 控制 Cubelet 采集频率，Prometheus `scrape_interval` 控制样本持久化频率，应分别检查两处配置。 |

Cubelet 启动失败时，先查看 systemd 日志；服务已运行但采集异常时，查看 Cubelet 业务日志：

```bash
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager
sudo tail -200 /data/log/Cubelet/Cubelet-req.log
```
