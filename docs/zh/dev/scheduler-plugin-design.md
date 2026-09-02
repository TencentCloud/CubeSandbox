# CubeSandbox 集群调度性能评估与高性能调度插件系统 — 设计/修复文档

> 代码基线：`master @ c458228e`（本 PR 与 master 的 merge base）。文中行号锚点以本 PR
> 合并后的代码树为准（基线 + 本 PR 新增的注释）；本 PR 未改动的文件与 `master @ c458228e` 一致。

---

## 1. 现状分析（代码走读结论）

### 1.1 调度流水线

入口：`Select()`（`Select` 函数定义处，`CubeMaster/pkg/scheduler/schedule.go`），调用方为
`sandbox_run.go:474`（创建）与 `sandbox_migrate.go:40`（迁移）。

```
PreFilter ──► Filter（并行执行，取交集）──► Score（加权求和 → 归一化 → PostScore → 排序）──► Pick
   │              │ 失败时                        │
   │              ▼                              ▼
   │        BackoffSelect                   结果写回 selCtx
   ▼
健康检查/熔断/亲和性/MvmNum 上限/指标新鲜度   （prefilter.go）
```

- **PreFilter**（`pkg/selector/prefilter/prefilter.go`）：从 localcache 取可调度节点，剔除
  不健康、熔断、亲和性不匹配、MvmNum 超限、指标超时的节点。
- **Filter 接口**（`pkg/selector/filter/init.go:17`）：`Select(ctx) (NodeList, error) + ID()`。
  内置 6 个：`cpu`、`mem`、`template_locality`、`realtime_create_num`、`disk`、`thirtparty`。
  并行执行后取**交集**（`schedule.go:159` `parallelRunFilters`）。
- **Score 接口**（`pkg/selector/score/init.go:19`）：`Select + ID + Weight + Disable`。
  内置 4 个：`real_time_weighted_average`（实时配额余量加权）、
  `multi_factor_weighted_average`（异步预计算多因子）、`affinity_score`、`image_score`
  （镜像/模板本地性打分）。各插件得分 × 插件权重求和后按总权重归一化（`runScoreFilter`
  中的归一化，`schedule.go`）。
- **PostScore**（`pkg/selector/postscore/whilelistscore.go`）：白名单加/减分。注意它并非独立
  阶段，而是在 `runScoreFilter` 内部**归一化之后、`AllSortByScore()` 排序之前**调用
  （`runScoreFilter` 中的 `postScore` 调用点，`schedule.go`）；新 Pipeline 必须保持这一相对顺序，否则白名单调整会被排序忽略。
- **最终选择**：`selCtx.LeastRandomSelect(PrioritySelectNum)`（`schedule.go:66`）。其实际行为
  由两个配置项共同决定，**未显式配置时打分结果几乎被完全丢弃**：

  | 配置项 | 默认值 | 影响 |
  |---|---|---|
  | `least_select_name` | `random`（字段定义 `config.go:260`，缺省填充见 `preHandleScheduler`，`config.go`） | `randomSelect.Add()` 显式忽略 weight 参数（`random_select.go:21-23`），即便 `LeastRandomSelect` 传入了 `Score*1e6`（`selectcontext.go:165`）也不生效 |
  | `priority_select_num` | **`-1` = 不限制**（`preHandleScheduler` 中的缺省填充，`config.go`；`conf.yaml:78` 才显式写为 1） | `LeastNodes(-1)` 返回全部候选，而非 Top-N |

  两者叠加：**当 YAML 未显式设置 `priority_select_num`（即保持代码默认 `-1`）时，部署在
  全部通过 Filter 的候选节点中等概率随机选择**，Score 阶段的排序结果不产生任何影响。
  仓库自带的 `conf.yaml` 显式写了 `priority_select_num: 1`，按该配置部署则确定性取最高分
  节点、打分真实生效——退化只发生在沿用代码缺省值的部署上。这是"打分缺省时退化"的典型
  现象，也是本方案"基线可显著改善"的最直接依据——不是插件算得不好，而是算完没被使用。

  需要说明的是，加权路径本身是存在的：显式配置 `least_select_name: rw` 会走
  `weighted.RandW`，`sw`/`rrw` 同理，此时权重真实生效。所以这是**配置缺省值问题，而非实现
  缺失**。后续由 Profile 显式配置 `best` / `top_n_uniform` / `top_n_weighted`，把选择语义从
  两个互相纠缠的缺省值中解放出来，避免名称与行为不一致。

### 1.2 插件注册机制现状

- 注册方式是**包内静态 map + reflect 调用**：
  `filter/init.go:43` 的 `filters` map、`score/init.go:56` 的 `scores` map。
- 配置驱动：`conf.yaml` 的 `scheduler.filter.enable_filters` /
  `scheduler.score.enable_scorers` 按名字启用；权重在
  `scheduler.score.plugin_conf.<plugin>.weight` 与 `scheduler.score.resource_weights` 中
  （结构体见 `pkg/base/config/config.go:252` `SchedulerConf`、`config.go:638` `ScorePluginConf`）。
- overcommit：`scheduler.overcommit_ratio`（默认 CPU=3、Mem=2，见 `config.go:350-351`，
  可按 instance_type 覆盖，`config.go:359` `GetEffectiveOvercommitRatio`）。

**存在的扩展性问题：**
1. 新插件必须改 `pkg/selector/{filter,score}` 包内的 map —— 入侵式，第三方无法在自己仓库注册；
2. `ScorePluginConf` 是**固定字段结构体**，每加一个 score 插件就要改 config 结构体。
   现成的反面例证：`config.go:643` 已定义 `TemplateScore *TemplateScore` 字段并可被 yaml 解析，
   但 `score/init.go:56` 的注册表中**并没有对应的构造函数**——配置面存在、实现面缺失的死字段。
   配置结构与插件集合分离正是 §3.3 改用 per-plugin `args` 的直接动因；
3. 没有"策略 Profile"概念：切换一套策略要同时改 enable_filters / enable_scorers / 多个
   plugin_conf / resource_weights，无法一键切换、也无法按场景组合；
4. reflect 调用无编译期检查（`filter/init.go:32`、`score/init.go:39`），插件名拼写错误时
   `!fn.IsValid()` 直接 `continue` **静默跳过**（`filter/init.go:34`、`score/init.go:41`），
   实际生效策略与配置声明不一致却无任何告警；
5. 构造函数在配置缺失时直接 `panic`，且**并非个例——4 个 score 插件全部如此**：
   `realtimescore.go:28`、`affinityscore.go:24`、`imagescore.go:38`、`multifactorscore.go:24`。
   §3.2 要求工厂返回 error 而非 panic，需一并整改这 4 处。

### 1.3 调度质量指标现状

- 仅有零散运行指标：`pkg/scheduler/local.go:239` `reportStdevTrace()` 每 10s 用
  `rcrowley/go-metrics` 计算节点 CPU/内存/MvmNum 使用率**标准差**（`local.go:259-261`）
  并通过 CubeLog 上报；没有装箱率、模板命中率、调度成功率、创建延迟分位数等统一指标定义。
- 没有针对调度器本身的 benchmark；`examples/cube-bench` 是**端到端**沙箱创建/删除延迟
  压测工具（打真实 CubeAPI），无法脱离集群复现"调度决策质量"实验。

---

## 2. 设计目标

| # | 设计目标 | 对应章节 |
|---|---|---|
| 1 | 指标定义文档，benchmark 输出 ≥5 项调度质量指标 | §3.1 指标定义 + §5 benchmark 报告 |
| 2 | 插件统一接入流水线，配置文件可切换策略 Profile | §3.2 注册表 + §3.3 Profile 与 per-plugin 配置 |
| 3 | 提供 ≥3 种内置调度策略，各有适用场景说明与配置示例 | §4.1 ~ 5.3 |
| 4 | 提供用户自定义插件开发示例与文档 | §4.4 |
| 5 | benchmark 含 ≥3 种 workload，一键运行生成对比报告 | §5.2 |
| 6 | 新策略至少一项指标明显改善，trade-off 需说明 | §5.3 报告 + §8 参考阈值 |
| 7 | 单测覆盖新增插件核心逻辑，PR 过代码规范检查 | §6、§7 |

---

## 3. 总体设计

### 3.0 设计思路

本方案的设计目标是"**在现有 `filter.Selector` / `score.Selector` 接口基础上**，提供可扩展的
调度插件注册与配置机制"。因此**不新建插件接口**：两个 `Selector` 接口保持原样，新插件与内置
插件实现同一接口，不存在 legacy 与新 API 的双轨。

现有接口已经具备所需的三项能力，缺口全部在**注册与配置层**：

| 能力要求 | 现有接口 | 缺口 |
|---|---|---|
| 启用/禁用插件 | `score.Selector.Disable()` 已定义并被 4 个插件实现 | 无（Profile 直接复用） |
| 调整权重 | `score.Selector.Weight()` 已定义，`runScoreFilter` 已加权求和并归一化（`schedule.go`） | 无（Profile 直接复用） |
| 组合为策略 Profile | — | 配置层缺 Profile 分组 |
| 插件注册 | 包级 map + reflect（`filter/init.go:32,43`、`score/init.go:39,56`） | 换成显式注册表 |
| 插件参数 | 固定字段 `ScorePluginConf`（`config.go:638`） | 换成 per-plugin `args` |

即 `Weight()` / `Disable()` 本来就是为可配置化设计的，只是上层没有对应的配置结构。三项改造
互相独立，均不触及 `Selector` 接口签名：

1. **注册表**（§3.2）：显式 `Register`/`New` 取代 reflect，未知插件名 fail fast，构造失败返回
   error 而非 panic；
2. **per-plugin `args`**（§3.3）：取代固定字段结构体，新增插件不必再改 config 结构；
3. **Profile**（§3.3）：命名策略组合，一处配置切换整套 filter/score/权重/picker。

自定义插件的接入方式为"**实现标准接口并注册即可**"：在自己的包里实现
`filter.Selector` 或 `score.Selector`，调用 `Register` 注册，在装配处 import 该包，
重新构建即可生效。不要求动态热加载。

架构：

```text
conf.yaml (profiles)                 CubeMaster
┌──────────────────┐              ┌──────────────────────────────────────┐
│ active_profile   │─────────────►│ ProfileCompiler                      │
│ profiles[]       │   启动期编译  │  查名 → 解码 args → 校验 → 实例化     │
└──────────────────┘              └────────────────┬─────────────────────┘
                                                   │ 产出 []Selector
用户插件包                                          ▼
┌──────────────────┐  Register    ┌──────────────────────────────────────┐
│ 实现 Selector    ├─────────────►│ Registry (filter / score)            │
│ init() 或显式注册 │              └────────────────┬─────────────────────┘
└──────────────────┘                               │
                                                   ▼
┌────────────────────────────────────────────────────────────────────────┐
│ PreFilter → Filter → Score(→归一化→PostScore→排序) → Pick → Metrics    │
└────────────────────────────────────────────────────────────────────────┘
```

### 3.1 调度评估指标体系

沿用 CubeMaster 已有 Prometheus 暴露链路，在 `CubeMaster/pkg/scheduler/metrics.go` 集中定义
指标；速率和分位数由 PromQL 从 counter/histogram 推导，不再重复维护易漂移的 rate gauge。

| Prometheus 指标 | 类型 | 定义与用途 | 采集点 |
|---|---|---|---|
| `scheduler_attempts_total{profile,result,reason}` | Counter | 调度尝试数；`result=success|failure`，失败原因使用有限枚举 | Pipeline 出口 |
| `scheduler_duration_seconds{profile}` | Histogram | 单次调度耗时，可计算 P50/P95/P99 | Pipeline 出口 |
| `scheduler_decisions_total{profile,template_hit}` | Counter | 成功决策数及模板就绪副本命中数 | Pick 后 |
| `scheduler_reschedules_total{profile,reason}` | Counter | 重试/重调度次数 | `loopRetry` 分支 |
| `sandbox_create_attempts_total{profile,result}` | Counter | Cubelet 创建成功/失败次数 | 创建链路出口 |
| `sandbox_create_duration_seconds{profile,result}` | Histogram | 请求受理至 Cubelet 返回的端到端耗时 | 创建链路出口 |
| `scheduler_cluster_quota{resource,type}` | Gauge | 集群原始 `allocated` 与 overcommit 后 `capacity`，二者分别输出 | 10s 节点快照任务 |
| `scheduler_node_load_cv{resource}` | Gauge | 节点使用率变异系数，衡量均衡度 | 10s 节点快照任务 |
| `scheduler_active_nodes` / `scheduler_empty_nodes` | Gauge | 当前承载沙箱与空节点数量，衡量装箱效果 | 10s 节点快照任务 |
| `scheduler_fragmented_capacity_ratio{shape}` | Gauge | 位于无法容纳基准规格节点上的空闲资源占比；`shape` 为有限配置集 | 10s 节点快照任务/benchmark |

统一口径：

- CPU/内存利用率 = `Σ allocated / Σ effective capacity`。观测值始终使用原始已分配量，
  不能用会受 `ignore_redis_allocation` 影响而归零的 `EffectiveAllocated()`；
- 负载 CV 使用总体标准差 `σ/μ`：无节点时不输出，单节点或均值为 0 时记为 0，
  全部使用 `float64`；
- 模板命中率 = `sum(rate(scheduler_decisions_total{template_hit="true"})) /
  sum(rate(scheduler_decisions_total))`，仅统计带 TemplateID 的请求；
- 成功率、重调度率同样由 counter 推导，不能把进程重启前后的瞬时比率持久化为 gauge；
- “集群平均利用率”无法单独证明装箱策略更好，因为同一 workload 的总分配量基本固定；
  装箱实验以 active/empty nodes、碎片率和剩余可调度请求数为主指标。

指标定义、PromQL 示例和 label 基数约束分别落到
`docs/dev/scheduler-metrics.md` 与 `docs/zh/dev/scheduler-metrics.md`。

### 3.2 插件注册表

`Selector` 接口不变，只把"名字 → 实例"的构造方式从 reflect 换成显式注册表。
`filter` 与 `score` 各一份，结构同构：

```go
// CubeMaster/pkg/selector/filter/registry.go（新增）
package filter

// Factory 由插件提供；args 为该插件在 Profile 中的私有配置，未配置时为空。
// 配置非法必须返回 error，不允许 panic。
type Factory func(args json.RawMessage) (Selector, error)

func Register(name string, f Factory) error
func New(name string, args json.RawMessage) (Selector, error)
func Names() []string   // 排序返回，便于诊断
```

`score` 包同构，`Factory` 返回 `score.Selector`（含 `Weight()` / `Disable()`）。

内置插件在各自包的 `init()` 中注册，`filters` / `scores` 两个 map 随之删除：

```go
func init() {
    Register("cpu", func(json.RawMessage) (Selector, error) { return NewCpuFilter(), nil })
    Register("template_locality", ...)
}
```

相对现状的改进，逐条对应 §1.2 列出的问题：

- **编译期检查**取代 reflect：`Factory` 签名不符直接编译失败，不再等到运行期
  （现状 `filter/init.go:32` `reflect.ValueOf` + `fn.Call(nil)`）；
- **未知插件名 fail fast**：现状 `!fn.IsValid() → continue` 静默跳过
  （`filter/init.go:34`、`score/init.go:41`），导致实际策略与配置声明不一致且无告警；
  改为启动期返回 error 并终止，配置写错必须立刻可见；
- **构造失败返回 error 而非 panic**：整改 4 处（`realtimescore.go:28`、`affinityscore.go:24`、
  `imagescore.go:38`、`multifactorscore.go:24`）；
- **第三方插件无需改 selector 包**：在自己的包里实现 `Selector` 并调用 `Register`，
  在装配处 import 即可。

注册表是包级的，与现状一致；单测通过 `Register` + 独立 `New` 调用即可覆盖，无需暴露实例级
容器。重复注册同名插件返回 error，在启动期暴露。

### 3.3 Profile 与 per-plugin 配置

Profile 不直接复用固定字段的 `ScorePluginConf`，每个插件拥有独立 `args`：

```yaml
scheduler:
  active_profile: template-hotstart
  profiles:
    - name: template-hotstart
      filters:
        - name: cpu
        - name: mem
        - name: disk
      scores:
        - name: image_score
          weight: 4
          args:
            # 因子名沿用 constants.go:241-260 的既有取值，避免与 legacy 配置产生两套命名
            factors: { template_id: 1 }
        - name: real_time_weighted_average
          weight: 1
          args:
            factors: { realtime_create_num: 3, quota_cpu_usage: 1, quota_mem_usage: 1 }
      picker: { mode: best }
```

> **因子命名必须与现有常量一致**（`pkg/base/constants/constants.go:241-260`，
> 消费点 `pkg/selector/score/utils.go:18-53`）：`realtime_create_num`（非
> `real_time_create_num`）、`quota_cpu_usage` / `quota_mem_usage`（非 `quota_cpu` / `quota_mem`）、
> `mvm_num`、`cpu_util`、`image_id`、`template_id`。legacy translator 依赖这套名字做双向映射，
> 本方案不改名。

与 legacy 的结构差异：现状各 score 插件配置中的 `EnableWeightFactors []string` 字段
（`config.go`，每个插件结构体各一份）只声明启用哪些因子，权重统一从全局
`Score.ResourceWeights map[string]float64`（`config.go:633`）查
（`utils.go:66-76` `getFactorWeight`）——即**因子的启用与权重被拆在两处、且权重全局共享，
不同 scorer 无法对同一因子取不同权重**。Profile 的 `args.factors` 把二者合并为 per-plugin
的 `map[string]float64`，legacy translator 负责把旧的 `[]string` + 全局 map 翻译成等价的
per-plugin map，保证默认行为不变。

启动时由 `ProfileCompiler` 按 `active_profile` 查表，对每个插件执行：注册表查名 → 解码
`args` → 校验（权重范围、因子名合法性）→ 调用 `Factory` 实例化，产出 `[]filter.Selector` 与
`[]score.Selector`，交给现有 `runFilter` / `runScoreFilter` 执行。任一步失败即启动失败。

`Weight()` 的取值来源随之统一：Profile 中的 `weight` 字段在实例化时传入插件，
`Selector.Weight()` 返回该值，`runScoreFilter`（`schedule.go`）的加权归一化逻辑不变。

兼容策略：未配置 `profiles` 时，legacy translator 把现有
`enable_filters` / `enable_scorers` / `plugin_conf` / `resource_weights` 翻译成等价的
`legacy-default` Profile，默认行为逐字节不变。

Profile 变更需重启 CubeMaster 生效。

Profile 的 picker 支持：`best`、`top_n_uniform`、`top_n_weighted`。

`legacy-default` Profile 的 picker **必须由 `least_select_name` 与 `priority_select_num`
两个配置项联合翻译**，不能简单固定为某一种（§1.1）：

| legacy 配置 | 等价 picker |
|---|---|
| `least_select_name: random` + `priority_select_num: -1`（**缺省组合**） | `top_n_uniform` with N = 全部候选 |
| `least_select_name: random` + `priority_select_num: N` | `top_n_uniform` with N |
| `least_select_name: rw`/`sw`/`rrw` + `priority_select_num: N` | `top_n_weighted` with N |

翻译错误会悄悄改变结果分布，因此 §6 的 legacy golden test 必须覆盖上表全部组合。

### 3.4 调度链路埋点

- `Select()` 出入口用 defer 记录 attempts 和 duration，失败原因映射到有限枚举；
- Pick 后判断模板命中并计数；
- 创建链路记录端到端 create counter/histogram；
- 10s 周期任务（`local.go:239` 现有链路）从同一节点快照计算 quota、CV、active nodes
  和 fragmentation；
- 热路径只做固定 label 的 Counter/Histogram 观测，不记录 node_id/template_id，
  防止 Prometheus 时序爆炸。注：现有 `cube_snapshot_storage_mode`
  （`pkg/templatecenter/snapshot_metrics.go:29-32`）已使用 node 级 label，本方案不扩大该模式。

> 本方案不涉及资源预占。#695 所述并发超卖需要独立的原子 reservation/rollback 机制，
> 属于并发正确性问题，与调度策略质量正交，不在本方案范围内。

---

## 4. 内置调度策略

三个策略均为 Profile 配置；尽量组合现有插件，只在缺少能力时新增插件。

### 4.1 `burst-spread`：突发短生命周期沙箱

- **场景**：AI Agent 并发拉起大量秒级至分钟级沙箱，优先避免单节点创建风暴；
- **Filter**：`cpu`、`mem`、`disk`、`realtime_create_num`；不使用模板硬亲和；
- **Score**：复用已有 `real_time_weighted_average`，提高 `realtime_create_num`、
  `mvm_num`、`cpu_util` 权重（因子名见 §3.3）。原方案的 `spread_score` 与现有实时打分
  公式重复，不新增；
- **Picker**：`top_n_weighted`，N=3~5，在保持质量的同时减少同分热点；
- **权衡**：预期负载 CV 下降，但模板本地命中率和缓存复用可能下降。

### 4.2 `template-hotstart`：同模板高频热启动

- **场景**：RL 训练、批量评测等重复创建同一模板，优先缓存命中和启动延迟；
- **Filter**：默认只保留资源、磁盘与并发硬约束，不用 `template_locality` 硬过滤；
- **Score**：提高现有 `image_score` 的 `template_id` 因子权重，并用
  `real_time_weighted_average` 在副本节点间均衡。原方案再增加 `template_affinity_score`
  会与 `image_score` 重复，因此删除；
- **权重需实验确定**：`calculatePriority`（`imagescore.go`）把**聚合**镜像得分截断到
  `[23MB, 80GB × 容器数]` 后线性映射到 0~100；每个已缓存镜像贡献
  `镜像大小 × 集群散布度`（`localcache.scaledImageScore`）。单模板场景下命中得分 ≈
  `100 × (镜像大小 × 散布度 − 23MB) / (80GB − 23MB)`：~5GB 模板全集群散布也只有
  ≈6/100（`template_storm` 计划的 30% 副本散布下更低，≈2/100），仅接近 80GB 的镜像
  才饱和，未命中为 0。即本地性是偏弱、随镜像大小与散布度缩放的连续信号，而非
  "命中满分"，原先"权重须落在较窄区间"的推断不成立。相反，要让本地性左右排序，
  `image_score` 权重需显著高于按 0~100 打分的实时资源因子（realtime_create_num、quota_*
  等，粗略估计高一个量级），且有效
  权重随模板镜像大小与散布度变化。该比值由步骤 3 的敏感性扫描（§7）在
  `template_storm` workload 上扫出，实验中固定并记录模板镜像大小与副本散布度，
  不预设固定值；权重过高仍会退化为近似硬过滤；
- **Picker**：`best`，使高权重本地性得分稳定生效；
- **严格模式**：可选 `template-hotstart-strict` Profile 加入 locality 硬 Filter。它可能获得
  接近 100% 的命中率，但资源不足时会降低成功率，必须单独报告，不作为默认值。

### 4.3 `binpack`：大规格长驻沙箱

- **场景**：大规格、长生命周期沙箱，目标是减少活跃节点与不可用碎片；
- **Filter**：`cpu`、`mem`、`disk`；
- **Score**：新增 demand-aware `binpack_score`，按请求放置后的剩余资源而非当前最高利用率
  计算 best-fit：

```text
cpuRemain = (cpuCapacity - cpuAllocated - requestCPU) / cpuCapacity
memRemain = (memCapacity - memAllocated - requestMem) / memCapacity
score = 100 × (1 - weightedMean(cpuRemain, memRemain))
        - imbalancePenalty × abs(cpuRemain - memRemain)
```

- **Picker**：`best`；分数相同按稳定 NodeID 破平，保证 benchmark 可复现；
- **权衡**：活跃节点/碎片率预期下降，负载 CV 会升高，节点故障影响面也会扩大。

策略 benchmark 必须固定相同 overcommit ratio；把 overcommit 与策略同时改变会产生混杂
变量，无法说明收益来自插件。生产 Profile 可以覆盖 overcommit，但报告需将其作为单独实验。

### 4.4 自定义插件开发示例

新增中英文 `docs/dev/scheduler-plugin-development.md` 与
`docs/zh/dev/scheduler-plugin-development.md`，配套示例代码
`CubeMaster/pkg/selector/examples/`，覆盖"实现标准接口并注册"的完整流程：

- **示例 1（Filter）`label_filter`**：按节点 label 过滤，约 40 行。
  实现 `filter.Selector` 的 `Select` / `ID`，在 `init()` 中 `filter.Register`，
  演示从 `args` 解码自定义参数；
- **示例 2（Score）`zone_spread_score`**：跨可用区打散打分，约 60 行。
  实现 `score.Selector` 的 `Select` / `ID` / `Weight` / `Disable`，
  演示权重如何由 Profile 注入、`Disable()` 如何响应配置；
- **接入步骤**（文档正文）：实现接口 → `Register` 注册 → 在装配处 import 插件包 →
  在 Profile 的 `filters` / `scores` 中按名启用并配 `args` → 重新构建 CubeMaster；
- 每个示例配 `_test.go`（表驱动 + fake NodeList），参考既有
  `filter/template_locality_test.go`、`score/imagescore_test.go` 的写法；
- 文档同时说明：`Select` 可能被并发调用，插件需自行保证线程安全；插件不应持有
  跨调度轮次的可变状态。

---

## 5. 调度 Benchmark 与实机验证

### 5.1 实机验证环境（效果数据以实机测量为准）

项目环境具备真实 CPU 资源，对比实验直接在真实集群上运行，效果数字全部来自实测，
不依赖离线仿真：

- **集群形态**：1 个 CubeMaster + ≥3 个 cubelet 节点（可用同机多 VM 或多 cubelet 实例，
  机器规格与节点清单写入报告）。CV、装箱率、活跃节点数、碎片率等**分布类指标必须
  多节点才有意义**，因此多节点集群是实验的前置条件，在实验记录中固定；
- **模板预置**：`template_storm` 需要的 30% 副本分布通过预部署实现，并写入实验记录；
- **对照一致性**：overcommit 等参数在所有 profile 间保持一致；切换策略 =
  改 `active_profile` + 重启 CubeMaster；
- **重复与记录**：每组（workload × profile）实验跑 ≥3 次，报告均值与离散程度；
  每份报告记录 Git commit、Profile 内容哈希、workload 参数、节点清单、Go 版本。

执行器：扩展 `examples/cube-bench`（已具备打真实 CubeAPI 的创建/删除压测能力），
新增三种 workload 的请求序列生成与报告输出：

- 请求序列（到达时刻、规格、寿命、TemplateID）由固定 seed **预生成**，
  同一 workload 的序列文件在所有 profile 间复用——各策略面对完全相同的负载流；
- 按序列中的寿命到期自动发起删除，形成分配/回收闭环；
- 一键入口 `make sched-bench`，输出 Markdown、JSON、CSV。

指标来源分两侧：执行器侧测**真实**端到端创建延迟 P50/P95 与请求成功率；
master 侧从 §3.1/§3.4 的指标链路（Prometheus `/metrics`）在实验窗口内采集
调度耗时 P50/P95、调度成功率、重调度率、模板命中率；
节点侧同窗口采集 CPU/Mem CV、装箱率、碎片率、活跃/空节点数。

需要的改造（单测与基准测试同样需要，与仿真无关）：`template_locality`
（`template_locality.go:54,96`）与 `realtime_create_num`（`realtimecreatelimit.go:40-48`）
目前直接调用 localcache 包级函数，需补上与 `imagescore.go:34`、`prefilter.go:26`
同款的函数指针注入点，使这两个插件在单测与 go bench 中可注入 fake 数据。
这是 §7 步骤 2 注册表改造的一部分。

### 5.2 三种内置 Workload

| Workload | 默认参数 | 模拟目标 |
|---|---|---|
| `burst` | 500 请求、泊松到达 50/s、寿命 U(10s,120s)、统一小规格 | 高并发短生命周期 |
| `template_storm` | 300 请求、同一 TemplateID、30% 节点预置副本 | 重复模板与缓存热点 |
| `mixed_spec` | 400 请求、1C2G:2C4G:8C16G=6:3:1、混合寿命 | 大小规格组合与资源碎片 |

实验矩阵 = workload × profile（legacy-default / burst-spread / template-hotstart / binpack）
× 重复次数（≥3）。请求规模与节点数按实际集群规模等比缩放并写入报告；
同一 workload 的预生成请求序列、节点初始状态和 overcommit 在所有 profile 间完全一致。

### 5.3 报告指标与调优建议

报告至少输出：**真实**端到端创建延迟 P50/P95、调度耗时 P50/P95、调度成功率、
模板命中率、CPU/Mem CV、CPU/Mem 装箱率、活跃节点数、空节点数、CPU/Mem 碎片率、
重调度率。所有延迟与成功率均为实测值，报告中不出现模型估算值。
关键表格同时给出绝对值、相对 legacy-default 变化、≥3 次重复的均值和离散程度，
避免用单次运行下结论。

自动建议仅基于显式规则生成，例如 CV 过高时提示增加实时余量权重、模板命中低且成功率
有余量时提示增加 `image_score` 权重。报告必须同时列出负向变化，不输出“全面优于默认”
这类无法由多目标调度实验支持的结论。

---

## 6. 测试与质量保障

| 层级 | 覆盖点 |
|---|---|
| 注册表单测 | 注册/重复注册报错、按名构造、未知名 fail fast、`Names()` 排序、Factory 返回 error 不 panic |
| Profile 编译测试 | legacy 翻译（含 §3.3 picker 映射表全部组合）、args 解码、权重边界、未知插件名启动失败 |
| 调度链路单测 | 各阶段顺序、PostScore 在归一化后排序前、取消、panic recovery、并发安全 |
| 策略单测 | 打分单调性、零配额、极端规格、dominant resource、稳定破平 |
| 指标单测 | counter 标签、CV 边界、利用率/碎片率公式、模板命中口径 |
| Benchmark 回归 | 请求序列固定 seed 可复现；每组实验 ≥3 次重复取均值与离散；单机 go bench 守住性能门槛 |
| 示例插件 | 两个示例插件的表驱动单测，与内置插件同一套 fake NodeList 写法 |

新增代码执行 `go test -race ./...`（资源允许的 package）、`go vet ./...`、
`golangci-lint run ./...` 和 `gofmt`。基准测试设置性能门槛：`legacy-default` Profile 在固定
100 节点用例中，调度吞吐下降不超过 5%，P95 额外开销不超过 5%；超限需 profile/pprof 解释。
该门槛的基线由步骤 1 搭建的单机基准用例产出（§7）。

---

## 7. 实施步骤

按依赖顺序分四步推进，每步形成可独立审查、可回滚的 PR，避免一次性提交大改动：

| 步骤 | 重点工作 | 产出与完成条件 |
|---|---|---|
| 步骤 1 | 设计评审与定稿；指标口径与埋点、cube-bench workload 扩展骨架先行 | 设计文档定稿；指标文档；cube-bench 可跑通单 workload 实机流程；单机基准用例产出性能门槛基线 |
| 步骤 2 | filter/score 注册表、localcache 注入点、4 处 panic 整改；per-plugin `args` + Profile + legacy translator | 注册表 PR 与 Profile PR 提交；未知插件名 fail fast；legacy 配置逐字节等价；默认行为 golden test 通过 |
| 步骤 3 | 三个 Profile、demand-aware binpack、权重敏感性扫描；cube-bench 三类 workload 与报告生成、实机矩阵实验 | 策略 PR 提交；一键生成 MD/JSON/CSV、固定 seed 可复现、输出 ≥5 指标；§4.2 权重区间由实机实验确定 |
| 步骤 4 | 参数实验收尾、中英文文档、示例插件、CI 收尾 | 对比报告、开发文档与示例、全部 CI 通过 |

步骤 2 的两个 PR 串行合入，步骤 3 的策略与 benchmark 可并行推进；若步骤 3 进度受挤压，
优先保证策略 PR 与对比报告，中英文文档与示例插件随后以独立 PR 补齐。

推荐 PR 顺序：指标/benchmark 基线 → 注册表 → Profile → 策略 → benchmark 报告 →
文档与示例。

PR 提交注意（遵循 `CONTRIBUTING.md` 与根目录 `AGENTS.md`）：

- commit message 使用英文 Conventional Commits 风格；
- **AI 不得添加 `Signed-off-by`**，由人类提交者自行签署 DCO；AI 辅助需在 commit/PR 中
  标注 `Assisted-by: <AGENT_NAME>:<MODEL_VERSION>`；
- PR 描述附设计文档、benchmark 原始配置和对比报告，方便复核结论。

---

## 8. 预期效果与风险

以下是评估用的参考阈值，不是脱离实验的性能承诺；最终以相同 workload、seed 和 overcommit 的
对照报告为准，至少一个新策略达到对应主目标且守住成功率门槛：

| Profile | 主目标（相对 legacy-default） | 守门指标 | 已知权衡 |
|---|---|---|---|
| `burst-spread` | `burst` CPU 或 Mem CV 相对下降 ≥10% | 成功率下降 ≤1 个百分点 | 模板命中可能降低 |
| `template-hotstart` | 命中率提升 ≥10 个百分点；实机创建 P95 同步观察 | 成功率下降 ≤1 个百分点 | 热点节点负载升高 |
| `binpack` | `mixed_spec` 活跃节点数或碎片率相对下降 ≥10% | 成功率下降 ≤1 个百分点 | CV 与故障影响面升高 |

工程效果：新增插件无需修改 selector 包，实现接口 + 注册即可接入；Profile 切换由一份配置
完成；错误插件名和错误参数在启动期暴露；benchmark 可复现实验且报告不少于五项质量指标；
新增插件有可运行的开发示例与中英文文档。

主要风险及应对：

- **改动波及现有调度行为**：注册表与 Profile 均为等价替换，`legacy-default` 由 translator
  保证逐字节兼容，golden test 覆盖 §3.3 picker 映射表全部组合；
- **模板本地性打分信号偏弱**：`calculatePriority`（`imagescore.go`）的聚合截断 + 线性映射
  使典型大小模板的命中得分仅为个位数（~5GB 全散布 ≈6/100，30% 散布 ≈2/100），而非满分；
  本地性容易被按 0~100 打分的实时资源因子淹没。§4.2 的权重比需由步骤 3 的
  敏感性扫描确定，扫描须固定并记录模板镜像大小与散布度，不预设固定值；
- **指标基数与性能**：禁止请求、节点、模板 ID 作为 label；通过 benchmark 守住 5% 门槛；
- **实机实验噪声与规模限制**：集群规模有限（≥3 节点）且存在环境噪声，每组实验
  ≥3 次重复并报告均值与离散程度；结论仅在报告记录的节点清单与参数下成立；
- **策略不存在全局最优**：每个 Profile 明确适用 workload 与负向指标，报告展示完整 trade-off。

