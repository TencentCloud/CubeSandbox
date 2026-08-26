# CubeMaster 调度器配置参考

本页说明 CubeMaster scheduler 的配置入口、节点选择流程、Cubelet 上报的数据如何参与调度，以及在 one-click / Terraform 部署中如何把节点数量、资源配额、并发和模板副本管理配置到位。

如果你只是想给多机部署启用基本评分，可以先阅读[多机集群部署](./multi-node-deploy.md#配置-cubemaster-调度评分)。如果需要排查调度失败、资源耗尽、节点标签不匹配或新增计算节点后的模板同步，请使用本页作为完整参考。

## 配置在哪里

CubeMaster 调度配置位于 CubeMaster 的 `conf.yaml`：

| 部署方式 | 配置位置 | 生效方式 |
|----------|----------|----------|
| one-click / systemd | `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml` | 修改后重启 `cube-sandbox-cubemaster.service` |
| 源码配置模板 | `configs/single-node/cubemaster.yaml` | 重新打包或拷贝到运行环境 |
| Tencent Cloud Terraform / TKE | `deploy/one-click/terraform/tencentcloud/tke-addons.tf` 中的 `kubernetes_secret.cubemaster_conf` | 修改 Terraform 通过 `yamlencode` 生成的配置，重新 apply Terraform，并重启或滚动更新 cube-master Pod |
| Kubernetes / Helm chart | `deploy/kubernetes/chart/files/cube-master/conf.yaml`，由 `deploy/kubernetes/chart/templates/master-config-secret.yaml` 渲染 | 修改 chart 文件或渲染后的 Secret，并重启或滚动更新 cube-master Pod |

Cubelet 节点元数据和资源配额不在 CubeMaster 配置里，而是由每台 Cubelet 上报。one-click 环境中的主要入口是：

| 配置 | 位置 | 说明 |
|------|------|------|
| Cubelet 静态配置 | `/usr/local/services/cubetoolbox/Cubelet/config/config.toml` | 包含 `node_status_update_frequency`，修改后重启 Cubelet |
| Cubelet 动态配置 | `/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml` | 包含 `host.scheduler_label` 和 `host.quota`，修改后重启 Cubelet |

常见 Cubelet 动态配置示例：

```yaml
host:
  scheduler_label: "default-cluster"
  quota:
    mcpu_limit: 0
    mem_limit: ""
    mvm_limit: 0
    creation_concurrent_num: 0
```

`0` 或空值通常表示让 Cubelet 按宿主机资源推导默认值，不等于无限资源。压测或大规模集群中如果要提高密度，应显式评估 CPU、内存、MVM 数量和并发创建上限。

## CubeMaster 如何选择计算节点

一次 sandbox 创建请求进入 CubeMaster 后，调度大致分为四步：

1. **解析请求约束**：读取 `instance_type`、模板 ID、资源规格、显式 host IP、node affinity / annotations 等条件。
2. **预过滤节点**：排除不健康、资源上报过期、超过 MVM 上限、模板本地副本不可用、实时或本地观测创建数过高、不满足 affinity 的节点；启用 `disk` filter 或 backoff 路径时，也会排除磁盘使用过高的节点。
3. **节点评分**：对过滤后的候选节点计算分数，例如基于 `mvm_num`、`local_create_num`、`quota_cpu_usage`、`quota_mem_usage` 做加权平均。
4. **最终选择**：从评分靠前的一组节点中选择。`priority_select_num` 控制进入最终随机选择的高分节点数量，`least_select_name` 默认为 `random`。

如果没有配置评分，CubeMaster 仍会做过滤，但可能按候选列表顺序选择节点，导致新 sandbox 更容易集中到第一个可用节点，直到资源过滤器把流量推向其他节点。

## 关键 scheduler 字段

推荐把下面的评分相关配置合并到现有 `cubemaster.yaml` 的 `scheduler` 段中。除非确实要替换，否则请保留已有的 `filter`、超时、overcommit 和实例类型专用配置。

```yaml
scheduler:
  # 保留当前部署已有的 filter、超时、overcommit 和其他 scheduler 配置。
  priority_select_num: 3
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

| 字段 | 作用 |
|------|------|
| `priority_select_num` | 从评分最高的前 N 个节点中做最终选择。多节点建议大于 `1`，小集群可从 `3` 开始。 |
| `metric_update_timeout` | 节点资源指标多久未更新后视为不可调度。应明显大于 Cubelet 上报周期。 |
| `local_metric_update_timeout` | 预留的本地指标超时字段。当前 prefilter 对全局指标和本地指标的新鲜度检查都使用 `metric_update_timeout`。 |
| `filter.enable_filters` | 启用调度过滤器。常见过滤器包括 CPU、内存、模板本地性和实时创建并发。 |
| `score.enable_scorers` | 启用评分器。多机部署通常启用 `real_time_weighted_average`；启用时必须同时配置 `score.plugin_conf.real_time_weighted_average`，否则 CubeMaster 可能在 scheduler 启动阶段 panic。 |
| `score.resource_weights` | 控制 MVM 数、创建并发、CPU/内存 quota 使用率等因子的权重。权重越高，该因子对分数影响越大；对应因子也必须列在 `score.plugin_conf.real_time_weighted_average.enable_weight_factors` 中。 |
| `overcommit_ratio` / `overcommit_ratio_conf` | 对 Cubelet 上报 quota 应用 CPU/内存超卖比例。默认 CPU 为 `3`、内存为 `2`，可按实例类型覆盖。 |
| `node_max_mvm_num` / `node_max_mvm_num_conf` | 全局或按实例类型限制单节点 MVM 数。Cubelet 上报的 `max_mvm_num` 也会参与实际上限计算。 |
| `disk_usage_max_percent` | `disk` filter 和 backoff 路径使用的磁盘水位阈值，用于避免继续调度到快满的机器。 |
| `affinityconf` / `node_affinity_selector_allowed_keys` | 控制按 cluster label、zone、CPU 类型、机型等做亲和或约束选择。 |

## 节点元数据如何影响调度

Cubelet 会通过 CubeOps 的 `/internal/v1/node-agent` 接口注册节点并持续上报状态。CubeOps 将这些数据持久化到 MySQL/Redis，CubeMaster 每隔几秒从 CubeOps 同步节点视图并维护本地缓存，调度时读取最新快照。

| Cubelet 上报字段 | 来源 | 调度影响 |
|------------------|------|----------|
| `instance_type` | Cubelet 节点身份 / 实例类型 | 用于匹配请求的 `instance_type`、按类型选择模板副本、套用按类型的 MVM/overcommit 配置。 |
| `cluster_label` | `host.scheduler_label` | 用于 cluster label 亲和、隔离不同节点池或指定模板分发范围。 |
| `quota_cpu` | `host.quota.mcpu_limit` 或宿主机推导值 | CPU 可调度容量的基础值，叠加 overcommit 后参与 CPU 过滤和评分。 |
| `quota_mem_mb` | `host.quota.mem_limit` 或宿主机推导值 | 内存可调度容量的基础值，叠加 overcommit 后参与内存过滤和评分。 |
| `max_mvm_num` | `host.quota.mvm_limit` 或按内存推导 | 单节点可承载的 MVM 数上限，超过后节点会被过滤。 |
| `create_concurrent_num` | `host.quota.creation_concurrent_num` | 节点上报的创建并发配置。`0` 表示 Cubelet 不设置额外 engine flow limit，但 CubeMaster 调度层仍会回落到 `cubelet_conf.create_concurrent_limit`。 |
| allocated / disk usage / cgroup metrics | Cubelet 周期上报 | 用于判断当前资源使用率、磁盘水位和评分因子。 |

这些值是每个计算节点独立配置和上报的。异构集群中，不同节点可以有不同实例类型、标签、配额和并发上限。

## 部署变量映射

### one-click 多节点

| 变量 / 配置 | 影响 |
|-------------|------|
| `ONE_CLICK_DEPLOY_ROLE=compute` | 安装计算节点，只运行 Cubelet 等运行时服务，并向控制面注册。 |
| `CUBE_SANDBOX_NODE_IP` | 当前节点注册到 CubeOps 的可路由地址。配置错误会导致节点不可达或不出现。 |
| `ONE_CLICK_CONTROL_PLANE_IP` / `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` | Cubelet 注册和上报使用的 CubeOps 地址（端口 3010）。CubeMaster 单独监听 8089。 |
| `Cubelet/config/config.toml` 中的 `node_status_update_frequency` | 节点状态和资源上报周期。默认 `1s`，不要配置到 dynamicconf。 |
| `Cubelet/dynamicconf/conf.yaml` 中的 `host.scheduler_label` | 节点池标签，用于 affinity 和隔离。 |
| `Cubelet/dynamicconf/conf.yaml` 中的 `host.quota.*` | CPU、内存、MVM 数、创建并发等调度容量。 |

### Tencent Cloud Terraform / TKE

| 变量 | 影响 |
|------|------|
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | PVM 计算节点数量，直接决定可承载 sandbox 的节点池规模。 |
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | 默认计算节点机型，影响真实 CPU/内存和 Cubelet 推导出的 quota。 |
| `TF_VAR_compute_instance_types` | 逐台指定异构计算节点机型。适合压测或混部场景。 |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | 每台计算节点 `/data/cubelet` 数据盘大小，影响模板、快照和运行时数据容量。 |
| `TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY` | Terraform 安装时写入每台计算节点的 Cubelet 静态配置，控制节点状态/资源上报频率。 |
| `TENCENTCLOUD_TKE_NODE_COUNT` / `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | 控制面 Pod 资源，不直接承载 sandbox，但会影响 CubeMaster、cube-api、cube-proxy 等控制面吞吐。 |

`TENCENTCLOUD_COMPUTE_NODE_COUNT` 和 `TENCENTCLOUD_TKE_NODE_COUNT` 是两套资源：前者运行 Cubelet 并承载 sandbox，后者运行控制面 Pod。

## 推荐配置

### 小测试集群

适合 POC、功能验证和少量并发。下面是新建小测试集群可用的完整起始配置，包含随项目提供的超时和过滤器默认值；如果是在已有部署上调整，请按需合并，不要盲目替换无关的 scheduler 配置。

```yaml
scheduler:
  priority_select_num: 3
  metric_update_timeout: 300s
  local_metric_update_timeout: 300s
  filter:
    enable_filters:
      - cpu
      - mem
      - template_locality
      - realtime_create_num
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

建议：

- 至少 2 台计算节点，便于验证节点选择和故障隔离。
- `priority_select_num` 从 `3` 开始；如果计算节点少于 3 台，可以设为节点数。
- Cubelet 上报周期保持默认 `1s`，CubeMaster 指标超时保持 `300s`。
- `host.quota` 使用默认推导值即可，但压测前必须显式检查是否过低。

### 较大生产类集群

适合更高并发或长期运行：

- 按节点池设置清晰的 `host.scheduler_label`，例如通用池、内存型池、压测池，避免不同用途互相挤占。
- 为不同 `instance_type` 配置 `node_max_mvm_num_conf` 和 `overcommit_ratio_conf`，不要把大规格节点和小规格节点套同一组上限。
- 适当增大 `priority_select_num`，通常可设置为健康计算节点数的一个小比例，但不建议大到完全随机。
- 保留 `template_locality` 过滤器，确保创建只落到已有模板副本的节点。
- 为 `host.quota.creation_concurrent_num` 设置明确上限，避免镜像、磁盘或 VMM 创建在单节点上被突发流量打满。
- 控制面也要扩容：提高 `TENCENTCLOUD_TKE_NODE_COUNT` 和控制面 Pod 副本数，避免 CubeMaster 或 cube-api 成为瓶颈。

## 新增计算节点后的 template redo

新增 compute node 后，节点注册成功并不代表所有模板都已经在该节点可用。调度器的 `template_locality` 过滤器会要求目标节点具备可用模板副本，否则创建请求可能失败或只调度到旧节点。

对镜像构建模板，新增节点后必须执行 template redo，把模板副本分发/重建到新节点：

```bash
cubemastercli tpl redo \
  --template-id <tpl-id> \
  --node <node-ip>
```

说明：

- `--node` 接受节点 ID 或 host IP；多个节点可以重复传入 `--node`。
- `redo` 默认会等待任务完成；如只提交任务可使用 `--detach`。
- 如果只想重做失败节点，可使用 `--failed-only`。
- redo 完成后再创建使用该模板的 sandbox，避免调度因模板不可用失败。

建议在多节点扩容流程中把 template redo 作为固定步骤：

1. 安装计算节点并确认它出现在 CubeMaster 节点列表。
2. 确认该节点健康、Cubelet 资源上报正常。
3. 对需要在该节点运行的模板执行 `cubemastercli tpl redo --template-id <tpl-id> --node <node-ip>`。
4. 等待 redo job 成功后，再放开业务流量或运行 E2E。

## 排障

### 调度失败或返回 no more resource

检查方向：

- `quota_cpu` / `quota_mem_mb` 是否低于实际创建需求。
- `host.quota.mcpu_limit`、`host.quota.mem_limit`、`host.quota.mvm_limit` 是否仍为默认推导值。
- `overcommit_ratio` 是否过低或按类型覆盖不符合预期。
- `mvm_num` 是否达到 `max_mvm_num` 或 `node_max_mvm_num_conf` 上限。
- 有效创建并发上限是否阻塞了当前节点。注意 `create_concurrent_num: 0` 在调度层仍会回落到 CubeMaster 的 `cubelet_conf.create_concurrent_limit`。

常用入口：

```bash
curl http://127.0.0.1:3010/internal/v1/nodes
sudo tail -F /data/log/CubeMaster/cubemaster-req.log
sudo tail -F /data/log/Cubelet/Cubelet-req.log
```

### 节点状态或资源上报过期

如果 CubeMaster 日志中出现 metric update timeout 类似信息：

- 确认计算节点 `cube-sandbox-cubelet.service` 正常运行。
- 确认 `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` 或 Terraform 生成的 CubeOps 地址从计算节点可达。
- 检查 `node_status_update_frequency` 是否被误改得过大。
- 确认 `metric_update_timeout` 明显大于上报周期。

### 模板不可用

典型现象是新节点加入后，创建仍只落到旧节点，或日志中提示模板本地副本不可用。

处理：

- 确认请求使用的 `template_id`。
- 对新增节点执行 `cubemastercli tpl redo --template-id <tpl-id> --node <node-ip>`。
- 查看模板 job 状态，确认 redo 成功。
- 保留 `template_locality` 过滤器，不建议为了绕过问题关闭它。

### Node label 不匹配

如果请求设置了 node affinity、cluster label 或特定 instance type，但没有候选节点：

- 检查 Cubelet `host.scheduler_label` 是否与请求或 `affinityconf` 中的标签一致。
- 检查 `instance_type` 是否与模板和请求匹配。
- 检查 `node_affinity_selector_allowed_keys` 是否允许请求使用的 selector key。
- 对异构节点池，确认模板副本已经 redo 到对应标签/机型的节点。

### 创建集中在少数节点

如果多机集群中新 sandbox 仍明显集中在一台机器：

- 确认 `score.enable_scorers` 已启用。
- 启用 `real_time_weighted_average` 时，确认 `score.plugin_conf.real_time_weighted_average.enable_weight_factors` 包含预期因子。
- 将 `priority_select_num` 设置为大于 `1`。
- 检查 `local_create_num`、`mvm_num`、`quota_cpu_usage`、`quota_mem_usage` 权重是否存在。
- 确认各节点模板副本都可用，否则 `template_locality` 会让候选节点集合变小。

## 相关文档

- [多机集群部署](./multi-node-deploy.md)
- [腾讯云集群部署（Terraform）](./tencentcloud-terraform-deploy.md)
- [服务管理与日志](./service-management.md)
- [模板相关排障](./troubleshooting/templates.md)
