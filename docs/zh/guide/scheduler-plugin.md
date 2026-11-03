# 可扩展调度插件

CubeMaster 按请求场景路由到调度 Profile。每个 Profile 由不可关闭的安全 Guards、可选 Filter、带权 Score、选点方式和失败策略组成。当前请求由哪个 Profile 服务,取决于[策略选择规则](./cubemaster-scheduler-config.md#当前生效的调度策略);本页说明如何配置与扩展 Profile。

## 升级行为变化

不修改调度配置直接升级的集群,受两处行为变化影响:

| 变化 | 影响对象 | 如何保持旧行为 |
|---|---|---|
| **空调度配置现在会激活出厂 Profile。** 此前零配置部署不做任何过滤和评分,从预过滤候选中随机选点;现在所有请求都经过强制 Guards,并由出厂 Score 按 spread 选点。注入发生时 CubeMaster 会输出告警日志。 | 既没有 `scheduler.profiles` 也没有 legacy `scheduler.filter` / `scheduler.score` / `scheduler.postscore` 的所有部署 | 显式设置 `scheduler.disable_factory_profiles: true`,或配置任一 legacy 配置块(会编译为兼容的 `default` Profile) |
| **模板亲和评分改为布尔 100/0 因子。** `image_score` 的 `template_id` 因子此前按节点上该模板本地副本的总大小经 `[23MB, 80GB]` 窗口线性打分;现在持有该模板的节点一律得 100,其余得 0,与暴露给 CEL 和 gRPC 插件的 `template_local` 事实语义一致。(旧的线性分值在 GiB 级模板下会被资源因子在数值上碾压,偏好实际已经很弱——但并非为零。) | 启用 `image_score` 的部署:出厂 `template_reuse` Profile(0.7 权重主导因子)及 `enable_scorers: [image_score]` 的 legacy 集群,模板请求落点可能明显变化 | 无退回开关;如落点变化不符合预期,调整 scorer 权重 |

## 出厂 Profile

二进制内置三条出厂 Profile(`CubeMaster/pkg/base/config/scheduler_factory.yaml`):

- `burst_balance`(label `workload=burst_balance`)——高并发同规格请求按资源水位与在途创建打散;
- `template_reuse`(label `workload=template_reuse`)——优先落在已持有该模板本地副本的节点;
- `mixed_binpack`(默认)——紧凑放置,保留整空节点。

当配置中既没有 `scheduler.profiles` 也没有 legacy 的 `scheduler.filter` / `scheduler.score` / `scheduler.postscore` 时,系统自动注入这套出厂策略,零配置部署也能按真实策略调度。一旦显式配置了其中任意一项,出厂策略整体不生效;仅配置 legacy 项时,系统仍把其编译为兼容的 `default` Profile,保持原有行为。

## Profile 配置

只有 `profile_route_label_keys` 中列出的请求 label 可以参与路由,也只有这些 label 会传给外部插件。非默认 Profile 必须包含 instance type 或 label 条件;路由按配置顺序匹配,第一个命中的 Profile 生效。

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

`selection.method` 决定如何从评分结果中选出最终节点:`highest` 严格选取评分最高的节点;`spread` 在评分最高的前 `top_n` 个候选中确定性选取当前运行沙箱数最少的节点(占用相同时保持评分顺序),用于把放置摊开;`random`(缺省)在前 `top_n` 个候选中按分数加权随机。`top_n: -1` 表示候选范围为全部通过过滤的节点。

自定义 Profile 固定执行 `node_safety`、`cpu`、`mem`、`disk`、`template_locality` 和 `realtime_create_num` Guards,配置不能关闭或重复声明这些安全约束。其中 `node_safety` 会在正常路径和 backoff 路径检查健康度、指标新鲜度、MVM 上限及 CPU load 合法性。

## 验证请求命中的 Profile

所有调度指标都携带路由到的 Profile 作为 `profile` 标签,最常用的检查:

- `scheduler_attempts_total{profile=...}`——哪个 Profile 在承接流量(未路由的失败落入 `profile="default"`);
- `scheduler_plugin_duration_seconds{profile,kind,plugin}`——逐插件耗时,调权重时有用;
- `sandbox_create_duration_seconds{profile,result}`——按 Profile 的端到端创建延迟。

## 失败语义

- Mandatory Guard 始终 fail-closed。
- Filter 默认 `fail-closed`;`fail-open` 必须显式配置,并会输出风险告警。
- Score 默认 `default-score`,单个插件失败后用其 `default_score` 继续;也可配置 `fail-closed`。
- `no_candidate` 支持 `fail` 和 `backoff`。自定义 Profile 使用 backoff 时仍会重新执行 Guards、Filter 和 Score。

配置在启动或热更新时整体编译;插件名、路由、表达式、权重、选点方式或失败策略无效时,新 Profile 集不会生效,调度器继续使用上一份完整管线。

## 准入与预留

创建路径不再维护预留账本,也不再执行同步 Redis reservation。跨副本准入是建议性的:指标上报窗口内多 Master 可能同时按同一份可见容量准入;节点实际装不下时,创建在第一次尝试直接失败,而不是转移到其他节点重试。旧配置中的 `scheduler.reservation_redis_error_policy` 已废弃,应删除。完整语义、升级注意事项与后续工作见[预留与 Redis](./multi-node-deploy.md#预留与-redis)。

## 内置 Score 与 legacy score 配置树

有三个内置 Score 的因子开关仍从 legacy 全局 `scheduler.score` 配置树读取,而不是 Profile 的 `args`:

- `real_time_weighted_average` 依赖 `scheduler.score.plugin_conf.real_time_weighted_average` 与 `scheduler.score.resource_weights`;
- `image_score` 依赖 `scheduler.score.plugin_conf.image_score`;
- `multi_factor_weighted_average` 依赖 `scheduler.score.plugin_conf.multi_factor_weighted_average`(其后台刷新协程也由该配置块驱动)。

Profile 引用了上述 Score 但缺少对应 legacy 配置块时,会在**编译期被拒绝**(启动或热更新时报错),不存在静默空转的 Score。零配置注入的出厂 Profile 在 `scheduler_factory.yaml` 中自带配套的 legacy `scheduler.score` 子树,原因正在于此——自定义出厂 Profile 时,权重改在 Profile 条目上,但因子开关仍需保留(或调整)该 legacy 子树。

## 插件类型

- `go`(默认):编译进 CubeMaster,通过统一 Registry 按名称注册。
- `expr`:启动时编译 CEL;Filter 必须返回 `bool`,Score 必须返回 0—100 的数值。
- `grpc`:连接独立进程,启动时完成协议/能力握手;请求超时、连续失败熔断、快照版本及返回节点/分数均由 CubeMaster 校验。

进程内 Go 插件实现现有 `filter.Selector` 或 `score.Selector` 接口,并在包初始化时调用 `plugin.RegisterGoFilter` / `plugin.RegisterGoScore`。CubeMaster 二进制需导入该包,因此新增 Go 插件后需要重新编译;重复名称会在启动时被拒绝。

CEL 提供基于版本化 protobuf 的强类型只读对象 `node` 与 `request`,未知字段、错误类型运算和不合法返回类型会在 Profile 激活时被拒绝。常用节点字段包括 `cpu_util`、`cpu_load`、`quota_cpu`、`allocated_cpu`、`quota_mem_mb`、`allocated_mem_mb`、`creating`、`local_creating`、`mvm_num`、`labels`、`local_templates`、`template_local` 和 `snapshot_storage_writable`;请求字段包括 `instance_type`、`cpu_millis`、`memory_bytes`、`system_disk_size`、`template_id` 和 `labels`。

外部插件配置示例:

```yaml
      filters:
        - name: company-policy
          type: grpc
          socket_path: /run/cube/company-scheduler.sock
          timeout: 100ms
          circuit_breaker_failures: 3
          circuit_breaker_cooldown: 30s
```

协议位于 `pkgs/proto/services/schedulerplugin/v1/plugin.proto`。启动时调用一次 `Handshake`,之后批量调用 `Filter` 或 `Score`。每个请求都携带其 `snapshot_version` 对应的完整冻结候选快照,因此插件服务端是无状态的,并发调度请求可以共享同一条连接而无需串行化。生产环境建议使用 Unix Domain Socket。可运行示例位于 `CubeMaster/examples/scheduler-plugin`:

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## 插件开发者契约说明

编写 Score 插件时有两点契约细节:

- 某个 Score 插件的评分维度对当前请求不适用时,可返回 `score.ErrNotApplicable` 显式跳过:不贡献分数与权重,也不按失败处理(即使在 `fail-closed` / `default-score` 策略下)。
- 相反,Profile 模式下返回空评分列表加 nil 错误属于违反插件契约,会触发配置的失败策略。内置的 legacy 耦合 scorer 在 legacy 因子配置缺失、被 disable、或启用因子权重全为零时也返回 `ErrNotApplicable`。
