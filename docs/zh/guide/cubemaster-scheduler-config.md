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

推荐把下面的评分相关配置合并到现有 `cubemaster.yaml` 的 `scheduler` 段中。除非确实要替换，否则请保留已有的 `filter`、超时和实例类型专用配置。

```yaml
scheduler:
  # 保留当前部署已有的 filter、超时和其他 scheduler 配置。
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
| `score.enable_scorers` | 启用评分器。多机部署通常启用 `real_time_weighted_average`；启用时必须同时配置 `score.plugin_conf.real_time_weighted_average`，否则 CubeMaster 可能在 scheduler 启动阶段 panic。对 `external_http_score` 同样适用：写入 `enable_scorers` 时必须提供匹配的 `score.plugin_conf.external_http_score`。 |
| `score.resource_weights` | 控制 MVM 数、创建并发、CPU/内存 quota 使用率等因子的权重。权重越高，该因子对分数影响越大；对应因子也必须列在 `score.plugin_conf.real_time_weighted_average.enable_weight_factors` 中。 |
| `score.plugin_conf.external_http_score` | 可选的 HTTP sidecar 评分器。见 [External HTTP score 插件](#external-http-score-插件)。 |
| `node_max_mvm_num` / `node_max_mvm_num_conf` | 全局或按实例类型限制单节点 MVM 数。Cubelet 上报的 `max_mvm_num` 也会参与实际上限计算。 |
| `disk_usage_max_percent` | `disk` filter 和 backoff 路径使用的磁盘水位阈值，用于避免继续调度到快满的机器。 |
| `affinityconf` / `node_affinity_selector_allowed_keys` | 控制按 cluster label、zone、CPU 类型、机型等做亲和或约束选择。 |

## 节点元数据如何影响调度

Cubelet 会通过 CubeOps 的 `/internal/v1/node-agent` 接口注册节点并持续上报状态。CubeOps 将这些数据持久化到 MySQL/Redis，CubeMaster 每隔几秒从 CubeOps 同步节点视图并维护本地缓存，调度时读取最新快照。

| Cubelet 上报字段 | 来源 | 调度影响 |
|------------------|------|----------|
| `instance_type` | Cubelet 节点身份 / 实例类型 | 用于匹配请求的 `instance_type`、按类型选择模板副本、套用按类型的 MVM 配置。 |
| `cluster_label` | `host.scheduler_label` | 用于 cluster label 亲和、隔离不同节点池或指定模板分发范围。 |
| `quota_cpu` | `host.quota.mcpu_limit` 或宿主机推导值 | CPU 可调度容量，参与 CPU 过滤和评分。 |
| `quota_mem_mb` | `host.quota.mem_limit` 或宿主机推导值 | 内存可调度容量，参与内存过滤和评分。 |
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
- 为不同 `instance_type` 配置 `node_max_mvm_num_conf`，不要把大规格节点和小规格节点套同一组上限。
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

## External HTTP score 插件

`external_http_score` 是可选评分插件。当它出现在 `score.enable_scorers` 中时，
CubeMaster 会把**当前候选节点列表**（经过 filter 之后）以 HTTP POST 发给运营配置的
sidecar，并把返回的逐节点分数并入加权总分。启用
`enable_scorers: external_http_score` **必须**同时提供匹配的
`score.plugin_conf.external_http_score`；否则 CubeMaster 在启动构造 scorer 时会
panic（与 `real_time_weighted_average` 相同）。

### 配置

当前 CubeMaster 在处理 `enable_scorers`（包括单独启用 `external_http_score`）
之前，要求 `score.resource_weights` 为非空映射。这是**加载器前置条件**，不是
HTTP 传输协议的一部分：若省略 `resource_weights`，CubeMaster 会构造空的
scorer 列表，sidecar **不会**被调用。下面的权重项是已有合法 key；
`external_http_score` 本身不会消费它。

```yaml
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      mvm_num: 1
    plugin_conf:
      external_http_score:
        weight: 1.0
        endpoint: "http://127.0.0.1:18080/score"
        timeout: 200ms   # 可选；为 0/省略时默认 200ms
        mode: ""         # 可选，原样转发给 sidecar
        disable: false
```

| 字段 | 含义 |
|------|------|
| `weight` | 在 `runScoreFilter` 加权平均（`Σ(score × weight) / Σ(weight)`）中的相对权重。返回分数必须与内置 scorer 使用相同的 **`[0, 100]`** 量纲；若 sidecar 返回归一化的 `0.0–1.0`，在相同 weight 下贡献大约只有内置 scorer 的 1%。**省略** `weight` 时，在配置加载 / 热更新（`preHandle`）阶段默认填为 **`1.0`**。**显式** `weight: 0` 表示插件仍启用但不贡献分数（与其他 scorer 一致，且跳过 HTTP 调用）。显式关闭请用 `disable: true`。负 weight 会在构造阶段被拒绝。每次 `Weight()` / `Select` 都会从 `plugin_conf` 热读（热更新无需重启）；`runScoreFilter` 在 `Select` **之前**只采样一次 `Weight()`，避免热更新落在一次尝试中间混入两代配置，或 weight 变为 0 后仍先打 HTTP。 |
| `endpoint` | Sidecar URL。为空则跳过该插件。非空时必须是带 host 的绝对 `http://` 或 `https://` URL；缺 scheme、`file://`、`unix://` 等会在构造时失败（热更新后则在 Select 上 fail-open）。请求前会 trim 首尾空白。密钥更宜放在 sidecar 侧；若 URL 含 userinfo 或 query token，scorer 不会记入日志，且 `config.Init` 的 cfg dump 只会保留 scheme/host/path。 |
| `timeout` | **同步 create 路径**上的单次 HTTP 超时。为 0/省略时使用默认 **200ms**。正值必须 **≤ 2s**；负值与超过 **2s** 的值会在构造 / Select 校验时被拒绝（不会被静默改写）。请使用 `200ms` / `1s` 这类 duration 字符串——裸整数如 `timeout: 200` 会被 YAML 解析成 **200 纳秒**。sidecar 卡住时，每次 create 最多会多等这么久再 fail-open。 |
| `mode` | 可选的运营自定义字符串，写入请求 JSON。 |
| `disable` | 为 true 时即使已 enable 也是空操作；与 `weight` 一样热读。若热更新删掉整个 `plugin_conf.external_http_score` 块但 `enable_scorers` 仍保留该名字，评分会停止，但会发出限流的 fail-open Warn，并递增 `plugin_conf_absent` 指标（scorer 实例在热更新后仍存活）。有意关闭请用 `disable: true`，或从 `enable_scorers` 去掉该名字。 |

### 传输协议

请求（`POST`，`Content-Type: application/json`）：

| 字段 | 单位 / 说明 |
|------|-------------|
| `mode` | 可选，来自配置。 |
| `instance_type` | 请求实例类型。 |
| `template_id` | 若存在则为请求模板 ID。 |
| `nodes[]` | filter 之后传给 scorer 的候选集合；每个节点一条。 |
| `nodes[].node_id` | 节点身份；请求中的每个候选都必须出现在 `scores` 中。 |
| `nodes[].quota_cpu` / `quota_mem` | 节点快照中的容量计数。 |
| `nodes[].quota_cpu_usage` / `quota_mem_usage` | **原始**上报占用计数（不经过 `EffectiveAllocated`）。当 `ignore_redis_allocation: true` 时，内置 scorer 可能把 allocated 视为 0，但这些传输协议字段仍携带 Redis 上报的原始值。 |
| 其他 `nodes[]` 字段 | `mvm_num`、创建计数、`cpu_util`、`mem_usage`、IP/类型等快照可用字段。 |

响应：

```json
{ "scores": { "node-a": 10.0, "node-b": 90.0 } }
```

- `scores` 必须包含**每一个**请求候选的 `node_id`。额外的 key 会被忽略（不会因此失败）；
  日志最多记录被忽略 key 的**数量**，不记录 key 名或响应正文。额外 key 上的 JSON
  `null` 或越界数值同样会被忽略。
- 已知候选的每个分数必须是**非 null** 的有限数值，范围 **`[0, 100]`**（数值 `0`
  合法；JSON `null` 不合法），越大越好（与内置 scorer 方向一致）。`scores` 下任意
  非数值 JSON（字符串、对象、数组）会在解码阶段视为畸形响应。
- 响应体超过 **1 MiB** 会被拒绝；**不跟随** HTTP 重定向。

### 失败 / 回退语义

scorer 失败（超时、非 2xx、重定向、畸形/过大响应、校验错误）会返回错误。
`runScoreFilter` 会跳过失败的 scorer 并继续调度（对 sandbox 创建保持
**fail-open**）。失败会递增 `cubemaster_scheduler_external_http_score_failures_total`，
并在 scorer 边界记录日志（不记录 endpoint URL、URL userinfo、query token，也不记录
请求/响应正文或密钥）。Warn 按脱敏后的失败类别大约每分钟至多一条（同类别后续失败
降为 Debug），避免 sidecar 宕机时刷爆 create 路径日志。任一请求候选缺少分数会使整次
尝试失败（反偏差：只给子集打分会系统性扭曲排序）。该调用在创建路径上是**同步**的。
共享 HTTP transport 用 `MaxConnsPerHost = 8`（与每 host 空闲池同级）限制对 sidecar
的在途连接，避免挂起时无界拨号风暴；每次尝试仍可能等待至多 `timeout`（默认 200ms，
上限 2s）再 fail-open。本 PR 不引入熔断、负缓存、异步执行或重试循环——更高 create
QPS 场景的后续工作。

## 相关文档

- [多机集群部署](./multi-node-deploy.md)
- [腾讯云集群部署（Terraform）](./tencentcloud-terraform-deploy.md)
- [服务管理与日志](./service-management.md)
- [模板相关排障](./troubleshooting/templates.md)
