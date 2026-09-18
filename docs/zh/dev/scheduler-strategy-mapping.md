# 调度策略 ↔ Workload ↔ 指标对应关系

> 本文是模块二（场景策略系统）的交付物：把三条内置场景策略与其对应的评测
> Workload、质量指标、运行开销指标及权衡关系固定下来，作为 Benchmark（模块三）
> 的评测口径。策略实现见 `CubeMaster/pkg/base/config/scheduler_factory.yaml`
> （go:embed 出厂配置）与 `CubeMaster/pkg/scheduler/profile/`；仿真口径见
> `CubeMaster/cmd/schedsim/README.md`。

## 总览

| 策略 | 路由 | 对应 Workload | 主要质量指标 | 运行与开销指标 |
|---|---|---|---|---|
| BurstBalance | labels `{workload: burst_balance}` | burst（突发短生命周期：500 请求、泊松 50/s、寿命 U(10s,120s)、小规格） | 羊群度、负载均衡（CV / Jain）、成功率 | 创建延迟、调度吞吐 |
| TemplateReuse | labels `{workload: template_reuse}` | template_storm（同模板风暴：300 请求、同一 TemplateID、30% 节点预置模板） | 模板命中率、同模板负载均衡、成功率 | 创建延迟、并发限制失败率 |
| MixedBinPack | default（兜底，无路由） | mixed_spec（混合规格：400 请求、1C2G:2C4G:8C16G=6:3:1、混合寿命） | 装箱率、资源碎片、成功率 | 调度延迟、单位请求计算开销 |

路由键由 `scheduler.profile_route_label_keys`（出厂值 `["workload"]`）控制；
未命中任何路由的请求落入 default 策略（MixedBinPack）。三类策略均由调度核心的
通用 Mandatory Guards + 并行 Filter + 并行 Score + Top-N 选点组合承载，
Guards 决定"能不能跑"，策略只决定可行节点之间的优先级。

## 零配置注入与 legacy 兼容

上述三条策略同时也是**零配置默认值**：当部署完全没有配置
`scheduler.profiles`、`scheduler.filter` 和 `scheduler.score` 时，
`preHandleScheduler`（`CubeMaster/pkg/base/config/config.go`）会注入内嵌在
`scheduler_factory.yaml` 中的出厂三 Profile。此时调度行为由出厂策略决定
（mandatory guards + 出厂 scorer + spread 选点），**不等同于**引入 Profile
机制之前的旧静态注册行为。

要保留旧行为，只需显式配置任一 legacy 键
（`scheduler.filter.enable_filters` 和/或 `scheduler.score.enable_scorers`）：
注入是 all-or-nothing 的，此时整体跳过，`profile.Compile` 会通过
`compileLegacy` 从旧静态配置重建流水线，保留旧的容错语义（未知插件名、
零权重 scorer 均跳过）。

兼容性由两级测试锁定：

- 编译层：`CubeMaster/pkg/scheduler/profile/profile_test.go` 中的
  `TestLegacyCompileSkipsUnknownPluginsAndZeroWeights` 与
  `TestProfileRoutingAndLegacyFallback`；
- 同输入选点：`CubeMaster/pkg/scheduler/profile_compat_test.go` 中的
  `TestLegacyAndExplicitProfileSelectIdentically` 用相同节点状态与同一请求
  分别执行 legacy 编译产出的流水线和语义等价的显式 Profile 流水线，
  断言两边每节点最终分数、排序与选中节点完全一致。

## BurstBalance（突发均衡）

**Profile 组成**（`burst_balance`）：

- Score：`real_time_weighted_average`（weight 1.0，配额水位）
- 选点：`spread` top_n=3，在高分候选间按运行沙箱数打散
- 失败策略：filter fail-closed / score default-score / no_candidate backoff

**目标 Workload**：burst。高并发同规格请求的资源水位在选点前几乎相同，
打散选点（top_n=3、按运行沙箱数最少）负责把突发放置摊开，而不是堆在同一节点。

**质量指标**：

- 羊群度（`herding_top1_share`）：被选中次数最多的节点占比，越低越好
- 负载均衡：节点用量率变异系数（`load_cv_cpu`/`load_cv_mem`）与 Jain 指数
  （`jain_cpu`/`jain_mem`）
- 成功率（`success_rate`）

**运行开销指标**：端到端创建延迟 P50/P95
（`sandbox_create_duration_seconds`）、调度吞吐
（`scheduler_duration_seconds` 反推）。

**权衡**：打散放置会触达更多冷节点，每个新触达节点付出一次冷启动
（模板未命中）代价；装箱率与模板命中率相比集中放置可能下降。实测见
[调度策略基准报告](/zh/dev/scheduler-eval-benchmark)。

## TemplateReuse（模板复用）

**Profile 组成**（`template_reuse`）：

- Score：`image_score`（weight 0.7，模板本地性：节点持有该模板本地副本得满分）+
  `real_time_weighted_average`（weight 0.3，资源水位兜底）
- 选点：`spread` top_n=3
- 失败策略：filter fail-closed / score default-score / no_candidate backoff

**目标 Workload**：template_storm。本地性占主导时，同模板突发请求会在少数
副本节点上排队（副本内羊群）；top_n=3 在持有本地副本的高分候选间打散，
把突发摊到多个副本节点。

**质量指标**：

- 模板命中率（`template_hit_rate`）：成功放置中选中节点持有本地副本的比例
- 同模板负载均衡：持有该模板副本节点上的用量 CV / Jain
- 成功率

**运行开销指标**：创建延迟 P50/P95（模板命中直接决定冷启动比例）、
并发限制失败率（`realtime_create_num` guard 拒绝 / backoff 重试率，见
`scheduler_reschedules_total`）。

**权衡**：在副本节点间分散意味着部分请求落在"刚触达"的副本节点上，首个请求
仍承担缓存升温成本；当副本覆盖率很高（如 30% 预载）时，"偏好副本节点"本身
就会把放置摊到二三十台节点上，命中率提升但真实延迟可能变差——需要在
模板命中率与创建延迟之间按业务取舍。

## MixedBinPack（混合装箱）

**Profile 组成**（`mixed_binpack`，default）：

- Score：`resource_fit_score`（weight 0.7，放入请求后剩余资源越少分越高，
  `imbalance_penalty: 0.5` 惩罚 CPU/内存剩余比例失衡）+
  `real_time_weighted_average`（weight 0.3，反向拉住极端堆叠）
- 选点：`spread` top_n=2，Top-2 打散防 herds
- 失败策略：filter fail-closed / score default-score / no_candidate backoff

**目标 Workload**：mixed_spec。大小规格混合到达时，优先填充已使用节点、
保留整空节点，提高集群装箱率并降低碎片。

**质量指标**：

- 装箱率：`cpu_alloc_rate` / `mem_alloc_rate`（Σ 用量 / Σ 原始配额，时间平均）
- 资源碎片：`fragmentation_ratio`（放不下 trace 最大 shape 的节点空闲 CPU
  占总空闲 CPU 的比例）
- 活跃/空节点数：`active_nodes_avg` / `empty_nodes_avg`（整空节点可回收）
- 成功率

**运行开销指标**：调度延迟 P50/P95/P99（`scheduler_duration_seconds`）、
单位请求计算开销（插件耗时 / CubeMaster CPU）。

**权衡**：紧凑放置提高利用率，但热点与单节点故障影响面增大；在 3× 超卖下
中低负载时各节点配额剩余差异很小，`resource_fit_score` 分数聚集，实际分散度
受 top_n 与 `real_time_weighted_average` 权重影响，需用对照实验调权。

## 指标口径与出处

仿真（schedsim）消费 cube-bench 的 trace 文件——`schedsim --trace
trace.json --config conf.yaml`，trace 由 `cube-bench --workload
burst|template_storm|mixed_spec --dump-trace` 生成（`--workload` 是
cube-bench 的参数，不是 schedsim 的）——并由
`pkg/scheduler/sim/metrics.go` 离线汇总质量指标（定义见
`cmd/schedsim/README.md` 的指标表）。生产侧由
`CubeMaster/pkg/scheduler/metrics.go` 的 Prometheus 指标输出对应信号
（`scheduler_decisions_total{profile,template_hit}`、
`sandbox_create_duration_seconds{profile,result}` 等），标签固定用策略名。

两侧指标的名称与意图一致，评测结论因此可外推到生产，但若干定义在细节上
并不相同——不要把 schedsim 与 Prometheus 的数值当作可直接互换：

- **空集的 Jain 指数**：schedsim 将空输入定义为完全均衡（1）；生产侧
  gauge（`jainOfRatios`）对空集群上报 0。全零（全空闲）集合两侧都返回 1。
- **碎片率**：schedsim 的 `fragmentation_ratio` 只看 CPU，参照形状取 trace
  中最大请求规格（`free <= shape` 即视为装不下，与 Filter 的严格准入判断
  一致）；生产侧 `fragmented_capacity_ratio` 参照形状取集群级 MaxMvm 规格，
  空闲 CPU *或*内存任一低于形状即视为装不下，并取 CPU 与内存两个比例的
  平均。
- **羊群度窗口**：schedsim 的 `herding_top1_share` 是整轮运行中命中最多
  节点的占比；生产侧 `scheduler_herding_top1_share` 是最近 100 次成功调度
  决策的滚动窗口。

对照实验方法（同一输入、同一环境、不同策略）与三模式（真机实测为主、
仿真为辅、真机 A/B）见《Cube 最终方案》模块三；量化对比结果见
[调度策略基准报告](/zh/dev/scheduler-eval-benchmark) 与
[出厂策略负载压测报告](/zh/dev/scheduler-factory-loadtest)。
