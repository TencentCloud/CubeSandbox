# 可扩展调度插件

CubeMaster 支持按请求场景选择调度 Profile。每个 Profile 由不可关闭的安全 Guards、可选 Filter、带权 Score、选点方式和失败策略组成。未配置 `profiles` 时，系统把原有 `filter`、`score`、`postscore` 和 `priority_select_num` 编译为 `default` Profile，保持原有行为。

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

自定义 Profile 固定执行 `node_safety`、`cpu`、`mem`、`disk`、`template_locality` 和 `realtime_create_num` Guards，配置不能关闭或重复声明这些安全约束。其中 `node_safety` 会在正常路径和 backoff 路径检查健康度、指标新鲜度、MVM 上限及 CPU load 合法性。

`selection.method` 支持 `random`、`spread` 和 `highest`。目前 `spread` 与 `random` 等价：都从得分最高的前 `top_n` 个节点中均匀随机选点，只有 `highest` 始终选择得分最高的节点。自定义 Profile 未显式配置 `top_n` 时默认为 1，此时 `random`/`spread` 等价于确定性地选择最优节点；而 legacy `default` Profile 使用 `priority_select_num`。需要打散时请显式配置 `top_n`。

基于 label 的路由只在创建沙箱（create）路径生效。迁移（migrate）和恢复放置（restore placement）调度不会设置路由 label，因此按 label 路由的 Profile 在这两条路径上不可达，相关请求始终走 fallback 管线。但 instance type 路由会在每个调度请求上匹配：migrate 和 restore 流程不带路由 label，却仍参与 `instance_types` 匹配（restore 会设置实例类型；migrate 的实例类型为空，而 `.*` 同样可以匹配空串）。像 `instance_types: [".*"]` 这样的宽泛路由会把这些非创建流程引入 Profile，其中的 expr/gRPC 插件将看到空的或零值的请求。请把 `instance_types` 限定在真正需要路由的实例类型上；另外，当不带资源规格的请求到达 mandatory `cpu`/`mem` Guard 时，Guard 会 fail-closed——直接返回硬错误且不进入 backoff。

## 插件类型

- `go`（默认）：编译进 CubeMaster，通过统一 Registry 按名称注册。
- `expr`：启动时编译 CEL；Filter 必须返回 `bool`，Score 必须返回 0—100 的数值。
- `grpc`：连接独立进程，启动时完成协议/能力握手；请求超时、连续失败熔断、快照版本及返回节点/分数均由 CubeMaster 校验。

进程内 Go 插件实现现有 `filter.Selector` 或 `score.Selector` 接口，并在包初始化时调用 `plugin.RegisterGoFilter` / `plugin.RegisterGoScore`。CubeMaster 二进制需导入该包，因此新增 Go 插件后需要重新编译；重复名称会在启动时被拒绝。注意并发契约：现在所有管线（包括 legacy default 管线）都会在同一请求内并发执行各个 Filter 和 Score——每个插件都面向完整候选集运行，Filter 结果在全部 Filter 跑完后才取交集——不同请求之间也始终并发，因此注册的实现必须是只读且线程安全的——仓库内置插件均满足。引入插件系统之前的 legacy 路径是顺序执行 Score 的。

CEL 提供基于版本化 protobuf 的强类型只读对象 `node` 与 `request`，未知字段、错误类型运算和不合法返回类型会在 Profile 激活时被拒绝。常用节点字段包括 `cpu_util`、`cpu_load`、`quota_cpu`、`allocated_cpu`、`quota_mem_mb`、`allocated_mem_mb`、`creating`、`local_creating`、`mvm_num`、`labels`、`local_templates`、`template_local` 和 `snapshot_storage_writable`；`reserved` 为预留字段，当前恒为 0。请求字段包括 `instance_type`、`cpu_millis`、`memory_bytes`、`system_disk_size`、`template_id` 和 `labels`。

节点指标字段承载的是原始遥测值，可能超出名义值域——超卖节点的 `cpu_util` 可能超过 100。Score 结果落在 [0,100] 之外会被输出校验判为失败，因此形如 `100.0 - node.cpu_util` 这类从上限做减法的表达式应显式钳制，例如 `node.cpu_util > 100.0 ? 0.0 : 100.0 - node.cpu_util`。

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

协议位于 `pkgs/proto/services/schedulerplugin/v1/plugin.proto`。调用顺序为 `Handshake`、`SyncSnapshot`，再批量调用 `Filter` 或 `Score`。生产环境建议使用 Unix Domain Socket。可运行示例位于 `CubeMaster/examples/scheduler-plugin`：

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## 失败语义

- Mandatory Guard 始终 fail-closed。
- Filter 默认 `fail-closed`；`fail-open` 必须显式配置，并会输出风险告警。
- Score 默认 `default-score`，单个插件传输/调用失败后用其 `default_score`（本身默认为 0）继续；也可配置 `fail-closed`。这与 Filter 刻意方向相反——健康 fail-closed、质量 fail-open——而且除一行告警日志外是静默的：所有候选拿到相同的常数分，失败插件的排序贡献消失；若它是唯一的 Score，排序退化为候选顺序。对排序质量敏感的部署应显式配置 `failure.score: fail-closed`。缺陷类失败绝不会被替换：panic、nil 绑定、以及校验类失败（nil、空 id、重复或非候选节点、NaN/Inf 或 [0,100] 外分值、只覆盖部分候选）都意味着插件本身有缺陷，在任何 Score 策略下都 fail-closed，`default-score` 也不例外。内建 go Score 的全局 `Disable()` 开关对 Profile 引用的插件同样生效——被禁用的 Score 与 legacy 路径一样被跳过（expr/gRPC 插件的 `Disable()` 恒为 false，不受影响）。
- 返回空结果的内置 `go` Score（例如请求没有节点偏好亲和性时的 `affinity_score`，或资源权重不适用时的 `image_score`）视为「不适用」并被跳过——不报错，也不替换为 `default_score`。只覆盖部分候选节点的结果仍视为失败。
- `no_candidate` 支持 `fail` 和 `backoff`。Mandatory Guard 永不触发 backoff：Guard 失败（包括清空候选集合）始终快速失败。只有可选 Filter 或最终选点无候选时才进入 backoff；backoff 尝试会在放宽后的候选集合上重新执行 Guards、Filter 和 Score，其中重跑 Guards 是 backoff 尝试自身的安全保障。这也意味着集群饱和时的行为与 legacy 路径不同：legacy 管线在饱和时（例如 `realtime_create_num` 导致无可调度节点）会进入 backoff 选择器重试，而自定义 Profile 会立即让请求失败——`no_candidate: backoff` 不会软化 Guard 的结果。
- `no_candidate` 未配置时默认为 `fail`。legacy `default` 管线始终走 backoff，因此启用第一个自定义 Profile 会把「Filter 后无候选」从 backoff 重试变成硬 `SelectNodesNoRes` 失败——没有 backoff 尝试，也不会回退到 default 管线。接入 Profile 期间建议显式配置 `no_candidate: backoff`。
- 插件要表达「没有合适节点」时应返回空候选列表而不是返回错误：插件错误一律按内部错误处理，由该插件的 `failure` 策略裁决，永远不会被归类为 `no_candidate`——因此返回错误的 Filter 不会触发 `backoff`，只会让请求失败（或 fail-open）。
- 输出校验比引入插件系统之前更严格，且其中大部分现在对 legacy default 管线同样生效：Filter 结果中包含 nil、空 id、重复或非候选节点会让请求直接失败；Score 结果中包含这类条目或 NaN/Inf 值会使该 Score 的整个结果失效（在 legacy `ScoreSkip` 绑定下该 scorer 会被整体剔除，而旧路径会静默把这些条目并入聚合结果，陈旧节点甚至可能因此重新进入候选集）。Profile 编译出的 Score 还要求每个分数落在 [0,100] 内且覆盖全部候选。把第三方 `go` Score 加入 Profile 前请先确认其值域边界——并注意现有 `enable_scorers` 部署同样会套用更严格的处理。

配置在启动或热更新时整体编译；插件名、路由、表达式、权重、选点方式或失败策略无效时，新 Profile 集不会生效，调度器继续使用上一份完整管线。

## 运维注意事项

- 配置 `scheduler.profiles` 后，外部 gRPC 插件会在 CubeMaster 启动、编译 Profile 集时同步建连并完成握手。任何建连或握手错误都会中止 `InitScheduler` 并使进程退出——插件只是暂时不可用（尚未启动、正在重启或 socket 残留）也会导致整个 master 无法启动，包括完全不经过该插件的 default Profile 流量。这与热更新路径刻意不对称：热更新会拒绝损坏的 Profile 集并保留上一份管线。请保证所有已配置的外部插件先于 CubeMaster 就绪（通过 systemd、sidecar 或监督进程编排启动顺序），或者干脆不配置外部插件。
- 熔断器打开对整个 Profile 是硬失败，而不是无候选事件：连续失败达到 `circuit_breaker_failures`（默认 3）后，插件在 `circuit_breaker_cooldown` 窗口（默认 30s）内以 `ErrCircuitOpen` 快速失败，表现为 Filter 错误，`no_candidate: backoff` 无法挽救。插件抖动会让路由到该 Profile 的请求在一个个 cooldown 窗口内持续失败——每个窗口结束后只允许一次半开探测，探测失败立即重新打开熔断器。如果插件只是建议性的，配置 `failure.filter: fail-open`；否则按插件真实的恢复速度调整 `timeout`、`circuit_breaker_failures` 和 `circuit_breaker_cooldown`。
- 每个外部插件客户端在整个快照同步 + RPC 过程中持有互斥锁，刻意将经过该插件绑定的并发请求串行化。在默认 100ms `timeout` 下，一个性能劣化的插件会把受影响的创建路径限制在约 10 请求/秒，且 backoff 尝试会额外支付一次快照冻结与同步。请保持插件 RPC 足够快，按此上限规划容量，并把插件延迟视为调度延迟。
- 同一插件同时用作 Filter 和 Score 时会构建两个独立客户端——各自建连与握手、各自持有熔断器和已同步快照版本——因此一个请求要上传两次全量快照，两个熔断器的状态还可能漂移。按 (name, socket_path) 引用计数共享客户端是已知的后续优化方向。
- 快照版本号每个请求唯一（时间戳加序列号），插件侧快照缓存跨请求永不命中：每个触及 gRPC 插件的请求都会重传完整的冻结节点集，backoff 路径传两次。改为只在节点集变化时才递增的 epoch 是已知的后续方向。
- 冻结快照时会对每个候选节点（含 `LocalTemplates`）、请求规格和路由 label 做深克隆；legacy default 管线即使不运行 expr/gRPC 插件也付同样的分配成本。对不允许非本地镜像的模板请求，freeze 期间还会对每个候选执行一次 `GetImageStateByNode` 查找——此前该查找仅在启用 `template_locality` 过滤器时才会发生。调优前请先基准测量；按 Profile 是否实际使用 expr/gRPC 插件来条件化 freeze 是已知的后续方向。
- 热更新被拒绝时会保留上一份 Profile 集，但全局配置在 watcher 运行前已完成切换，而内建插件在 Select 时实时读取全局配置（Guard 超时、Score 的 `Disable()` 开关、`real_time_weighted_average`/`image_score` 配置段、`EffectiveQuota*` 包装）。因此被拒绝的热更新可能留下「旧管线跑新配置值」的状态——应把拒绝日志视为需要运维介入，而不是无影响事件。
- gRPC 与 CEL 的请求上下文中，`cpu_millis` 和 `memory_bytes` 都是普通整数，因此「未指定资源规格」与「零规格请求」无法区分——restore 放置路径会传入空规格。
- 各类插件的分数量纲并未统一：expr 与 gRPC Score 强制限制在 [0,100]，而内置 `go` Score 各有自己的值域——`real_time_weighted_average` 归一化到约 [0,1]，而 `image_score` 与 `affinity_score` 的分值可达约 100。在混用 `type: go` 与 `type: expr`/`grpc` Score 的 Profile 中，必须用各插件的 `weight` 吸收值域差异，否则值域更宽的插件会主导聚合结果。

## 兼容性说明

- 未注册为插件的 legacy `enable_filters` / `enable_scorers` 配置项现在会使 CubeMaster 启动失败，错误信息会指出具体配置项；此前这类条目会被静默跳过。升级前请从配置中移除失效条目，或先注册对应插件。
- weight 非正的 legacy `enable_scorers` 条目现在会在编译管线时被跳过并输出告警（此前是静默贡献 score×0）；调度行为不变。
- 上述未注册条目的启动失败只在 legacy 管线实际被编译时成立：一旦 `profiles` 下配置了默认条目，`enable_filters` / `enable_scorers` 就不再参与编译，失效条目会被静默忽略而非报错。迁移到 Profiles 时请一并清理这些条目。
- `multi_factor_weighted_average` 的 legacy 后台分数刷新协程现在只要对应配置段存在就会启动，即使该 scorer 未列入 `enable_scorers`；循环每个 tick 都会重读实时配置，因此对"已配置但未启用"的部署而言，唯一影响是多一个空转 goroutine。
- legacy default 管线现在与自定义 Profile 共用同一个并发运行器：Score 在请求内并行执行，且畸形的 Filter/Score 输出（nil、空 id、重复或非候选条目、NaN/Inf 值）会被拒绝，而不是像旧路径那样静默并入结果。仓库内置插件均满足该契约；升级前请审计任何第三方 `go` 插件。
- 配置 `profiles` 但没有 `default: true` 条目时，未命中任何路由的请求会回落到由 `enable_filters`/`enable_scorers` 编译出的 legacy 管线；若完全没有 legacy 配置段，该回落管线是无守卫的纯随机选择。编译期会输出一条含已编译 Filter/Score 数量的 RISK 告警——除非明确依赖该回落，否则请配置默认 Profile。
- 内置 `go` 资源过滤器（`cpu`、`mem`、`disk`）对未携带对应资源规格的请求会直接报错，与 legacy 管线行为完全一致（legacy 也是无条件运行它们的）。可能收到无规格请求（例如传入空规格的 restore 放置路径）的 Profile 不应把它们列为 Guard 或 Filter。
- 当完全未加载 Profile 集时（例如启动期请求早于 `InitScheduler` 完成），`Select` 现在会以"scheduler profile is not initialized"快速失败；此前它会回落到包级全局 Selector 列表，而新的初始化路径不再填充这些列表。
