# 可扩展调度插件

CubeMaster 支持按请求场景选择调度 Profile。每个 Profile 由不可关闭的安全 Guards、可选 Filter、带权 Score、选点方式和失败策略组成。

二进制内置三条出厂 Profile（`CubeMaster/pkg/base/config/scheduler_factory.yaml`）：`burst_balance`、`template_reuse` 通过请求 label `workload=burst_balance` / `workload=template_reuse` 选中，其余请求落入默认的 `mixed_binpack`。当配置中既没有 `scheduler.profiles` 也没有 legacy 的 `scheduler.filter` / `scheduler.score` / `scheduler.postscore` 时，系统自动注入这套出厂策略，零配置部署也能按真实策略调度。一旦显式配置了 `scheduler.profiles` 或 legacy filter/score/postscore 中的任意一项，出厂策略整体不生效；仅配置 legacy 项时，系统仍把 `filter`、`score`、`postscore` 和 `priority_select_num` 编译为兼容的 `default` Profile，保持原有行为。

## 升级行为变化

不修改调度配置直接升级的集群需要注意两处行为变化：

- **空调度配置现在会激活出厂 Profile。** 此前，既没有 `scheduler.profiles` 也没有 legacy `scheduler.filter` / `scheduler.score` / `scheduler.postscore` 的部署不做任何过滤和评分，从预过滤候选中随机选点。注入出厂策略后，所有请求都会经过强制 Guards（`node_safety`、`cpu`、`mem`、`disk`、`template_locality`、`realtime_create_num`），并由出厂 Score 按 `spread` / `top_n` 选点，放置决策与旧的随机选点不同。注入发生时 CubeMaster 会输出告警日志。如需保持旧行为，可显式设置 `scheduler.disable_factory_profiles: true`（显式关闭注入），或显式配置 legacy `scheduler.filter` / `scheduler.score`（会编译为兼容的 `default` Profile）或 `scheduler.profiles`。
- **模板亲和评分改为布尔 100/0 因子——这是一个真实的行为变化。** 此前 `image_score` 的 `template_id` 因子按节点上该模板本地副本的总大小线性打分，经 `[23MB, 80GB]` 窗口映射到 `[0, 100]`；现在持有该模板的节点一律得 100，其余节点得 0，与暴露给 CEL 和 gRPC 插件的 `template_local` 事实语义一致。（旧的线性分值在 GiB 级模板下会被资源因子在数值上碾压，模板亲和偏好实际上已经很弱——但并非完全没有影响。）落点实际变化多大取决于部署启用了哪些 Score 及权重：出厂 `template_reuse` Profile 中 `image_score` 是占 0.7 权重的主导因子；只启用 `image_score`（`score.enable_scorers: [image_score]`）的 legacy 集群也可能在不改配置的情况下看到模板请求落点的明显变化。

## Profile 配置

只有 `profile_route_label_keys` 中列出的请求 label 可以参与路由，也只有这些 label 会传给外部插件。非默认 Profile 必须包含 instance type 或 label 条件；路由按配置顺序匹配，第一个命中的 Profile 生效。

```yaml
scheduler:
  profile_route_label_keys: [workload]
  profiles:
    - name: burst
      route:
        instance_types: ["S.*", "M.*"]
        labels: {workload: burst}
      filters:
        - name: skip-high-create
          type: expr
          expr: "node.creating < 8"
      scores:
        - name: prefer-idle
          type: expr
          expr: "node.cpu_util < 60.0 ? 80.0 : 20.0"
          weight: 2
      selection: {top_n: 5, method: spread}
      failure:
        filter: fail-closed
        score: default-score
        no_candidate: fail
```

`selection.method` 决定如何从评分结果中选出最终节点：`highest` 严格选取评分最高的节点；`spread` 在评分最高的前 `top_n` 个候选中确定性选取当前运行沙箱数最少的节点（占用相同时保持评分顺序），用于把放置摊开；`random`（缺省）在前 `top_n` 个候选中按分数加权随机。`top_n: -1` 表示候选范围为全部通过过滤的节点。

自定义 Profile 固定执行 `node_safety`、`cpu`、`mem`、`disk`、`template_locality` 和 `realtime_create_num` Guards，配置不能关闭或重复声明这些安全约束。其中 `node_safety` 会在正常路径和 backoff 路径检查健康度、指标新鲜度、MVM 上限及 CPU load 合法性。

创建路径不再维护预留账本，也不再执行同步 Redis reservation。CubeMaster 使用本地节点快照执行准入并派发到 Cubelet；指标独立更新，上报窗口内多副本可能重复准入。旧配置中的 `scheduler.reservation_redis_error_policy` 已废弃，应删除。Redis 仍用于节点指标及创建成功后的代理元数据等功能。

多 Master 继续依赖上报指标和 `realtime_create_num` 的本地并发数乘 Master 数估算，但不再提供跨副本原子容量预留。指标传播期间可能出现并发超额接收。Cubelet 已有创建并发限流和单沙箱 cgroup 限额；节点总配额的原子接收控制属于后续工作，本次未实现。不能假定所有超额 CPU/内存请求都会被 Cubelet 拒绝。具体来说，节点实际装不下时创建会在第一次尝试直接失败，而不是转移到其他节点重试：Cubelet 目前不做 CPU/内存/MVM 配额准入，相应错误码也不在任何重试集合中。让这一拒绝成为可重试的故障转移属于后续工作。

## 插件类型

- `go`（默认）：编译进 CubeMaster，通过统一 Registry 按名称注册。
- `expr`：启动时编译 CEL；Filter 必须返回 `bool`，Score 必须返回 0—100 的数值。
- `grpc`：连接独立进程，启动时完成协议/能力握手；请求超时、连续失败熔断、快照版本及返回节点/分数均由 CubeMaster 校验。

进程内 Go 插件实现现有 `filter.Selector` 或 `score.Selector` 接口，并在包初始化时调用 `plugin.RegisterGoFilter` / `plugin.RegisterGoScore`。CubeMaster 二进制需导入该包，因此新增 Go 插件后需要重新编译；重复名称会在启动时被拒绝。

CEL 提供基于版本化 protobuf 的强类型只读对象 `node` 与 `request`，未知字段、错误类型运算和不合法返回类型会在 Profile 激活时被拒绝。常用节点字段包括 `cpu_util`、`cpu_load`、`quota_cpu`、`allocated_cpu`、`quota_mem_mb`、`allocated_mem_mb`、`creating`、`local_creating`、`mvm_num`、`labels`、`local_templates`、`template_local` 和 `snapshot_storage_writable`；请求字段包括 `instance_type`、`cpu_millis`、`memory_bytes`、`system_disk_size`、`template_id` 和 `labels`。

外部插件配置示例：

```yaml
      filters:
        - name: company-policy
          type: grpc
          socket_path: /run/cube/company-scheduler.sock
          timeout: 100ms
          circuit_breaker_failures: 3
          circuit_breaker_cooldown: 30s
```

协议位于 `pkgs/proto/services/schedulerplugin/v1/plugin.proto`。启动时调用一次 `Handshake`，之后批量调用 `Filter` 或 `Score`。每个请求都携带其 `snapshot_version` 对应的完整冻结候选快照，因此插件服务端是无状态的，并发调度请求可以共享同一条连接而无需串行化。生产环境建议使用 Unix Domain Socket。可运行示例位于 `CubeMaster/examples/scheduler-plugin`：

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## 失败语义

- Mandatory Guard 始终 fail-closed。
- Filter 默认 `fail-closed`；`fail-open` 必须显式配置，并会输出风险告警。
- Score 默认 `default-score`，单个插件失败后用其 `default_score` 继续；也可配置 `fail-closed`。
- `no_candidate` 支持 `fail` 和 `backoff`。自定义 Profile 使用 backoff 时仍会重新执行 Guards、Filter 和 Score。

配置在启动或热更新时整体编译；插件名、路由、表达式、权重、选点方式或失败策略无效时，新 Profile 集不会生效，调度器继续使用上一份完整管线。

## 内置 Score 与 legacy score 配置树

有三个内置 Score 的因子开关仍从 legacy 全局 `scheduler.score` 配置树读取，而不是 Profile 的 `args`：

- `real_time_weighted_average` 依赖 `scheduler.score.plugin_conf.real_time_weighted_average` 与 `scheduler.score.resource_weights`；
- `image_score` 依赖 `scheduler.score.plugin_conf.image_score`；
- `multi_factor_weighted_average` 依赖 `scheduler.score.plugin_conf.multi_factor_weighted_average`（其后台刷新协程也由该配置块驱动）。

Profile 引用了上述 Score 但缺少对应 legacy 配置块时，会在**编译期被拒绝**（启动或热更新时报错），不存在静默空转的 Score。零配置注入的出厂 Profile 在 `scheduler_factory.yaml` 中自带配套的 legacy `scheduler.score` 子树，原因正在于此——自定义出厂 Profile 时，权重改在 Profile 条目上，但因子开关仍需保留（或调整）该 legacy 子树。

当某个 Score 插件的评分维度对当前请求不适用时，可返回 `score.ErrNotApplicable` 显式跳过：不贡献分数与权重，也不按失败处理（即使在 `fail-closed` / `default-score` 策略下），例如只对带 `template_id` 的请求才有意义的维度。相反，Profile 模式下返回空评分列表加 nil 错误属于违反插件契约，会触发配置的失败策略。内置的 legacy 耦合 scorer（`real_time_weighted_average`、`image_score`、`multi_factor_weighted_average`）在 legacy 因子配置缺失、被 disable、或启用因子权重全为零时也返回 `ErrNotApplicable`；这些配置在 Profile 编译期就会被拒绝，因此该路径只在热更新改动 legacy 配置时触发。
