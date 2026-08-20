# CubeSandbox 集群调度性能评估与高性能调度插件系统 — 设计/修复文档

> 实战任务一（项目导师：龙进）
> 撰写日期：2026-08-19　对应代码基线：`master @ 4eb62321`

---

## 1. 相关 Issue / PR（背景参考）

- [#695](https://github.com/TencentCloud/CubeSandbox/issues/695) — Concurrent CreateSandbox has no lock — scheduling race can oversell VMs（并发创建时调度超卖风险，与本任务的"创建成功率/装箱率"指标直接相关）
- [#1156](https://github.com/TencentCloud/CubeSandbox/issues/1156) / PR #1157 — scarce-resource scheduler filter (SRA)，**是"新增一个调度 Filter 插件"的完整范例**，可作为本任务插件开发的对标参考
- [#573](https://github.com/TencentCloud/CubeSandbox/issues/573) — Scheduler binds to first sorted node when scoring is unconfigured（打分缺省时的退化行为）
- [#342](https://github.com/TencentCloud/CubeSandbox/issues/342) / #326 / #301 — Cubelet `/v1/metrics/scheduler` 节点配额指标上报链路
- [#1040](https://github.com/TencentCloud/CubeSandbox/issues/1040) — Top Contributor Program

---

## 2. 现状分析（代码走读结论）

### 2.1 调度流水线

入口：`CubeMaster/pkg/scheduler/schedule.go:28` `Select()`，调用方为
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
  并行执行后取**交集**（`schedule.go:157` `parallelRunFilters`）。
- **Score 接口**（`pkg/selector/score/init.go:19`）：`Select + ID + Weight + Disable`。
  内置 4 个：`real_time_weighted_average`（实时配额余量加权）、
  `multi_factor_weighted_average`（异步预计算多因子）、`affinity_score`、`image_score`
  （镜像/模板本地性打分）。各插件得分 × 插件权重求和后按总权重归一化（`schedule.go:246`）。
- **PostScore**（`pkg/selector/postscore/whilelistscore.go`）：白名单加/减分。注意它并非独立
  阶段，而是在 `runScoreFilter` 内部**归一化之后、`AllSortByScore()` 排序之前**调用
  （`schedule.go:251`）；新 Pipeline 必须保持这一相对顺序，否则白名单调整会被排序忽略。
- **最终选择**：`selCtx.LeastRandomSelect(PrioritySelectNum)`（`schedule.go:64`）。其实际行为
  由两个配置项共同决定，**默认配置下打分结果几乎被完全丢弃**：

  | 配置项 | 默认值 | 影响 |
  |---|---|---|
  | `least_select_name` | `random`（`config.go:249`，缺省填充见 `config.go:1207-1208`） | `randomSelect.Add()` 显式忽略 weight 参数（`random_select.go:21-23`），即便 `LeastRandomSelect` 传入了 `Score*1e6`（`selectcontext.go:161`）也不生效 |
  | `priority_select_num` | **`-1` = 不限制**（`config.go:1203-1204`；`conf.yaml:74` 才显式写为 1） | `LeastNodes(-1)` 返回全部候选，而非 Top-N |

  两者叠加：**默认部署在全部通过 Filter 的候选节点中等概率随机选择**，Score 阶段的排序结果
  不产生任何影响。这正是 issue #573（打分缺省时的退化行为）描述的现象，也是本任务"基线可
  显著改善"的最直接依据——不是插件算得不好，而是算完没被使用。

  需要说明的是，加权路径本身是存在的：显式配置 `least_select_name: rw` 会走
  `weighted.RandW`，`sw`/`rrw` 同理，此时权重真实生效。所以这是**配置缺省值问题，而非实现
  缺失**。后续由 Profile 显式配置 `best` / `top_n_uniform` / `top_n_weighted`，把选择语义从
  两个互相纠缠的缺省值中解放出来，避免名称与行为不一致。

### 2.2 插件注册机制现状

- 注册方式是**包内静态 map + reflect 调用**：
  `filter/init.go:43` 的 `filters` map、`score/init.go:56` 的 `scores` map。
- 配置驱动：`conf.yaml` 的 `scheduler.filter.enable_filters` /
  `scheduler.score.enable_scorers` 按名字启用；权重在
  `scheduler.score.plugin_conf.<plugin>.weight` 与 `scheduler.score.resource_weights` 中
  （结构体见 `pkg/base/config/config.go:241` `SchedulerConf`、`config.go:627` `ScorePluginConf`）。
- overcommit：`scheduler.overcommit_ratio`（默认 CPU=3、Mem=2，见 `config.go:339-340`，
  可按 instance_type 覆盖，`config.go:348` `GetEffectiveOvercommitRatio`）。

**存在的扩展性问题：**
1. 新插件必须改 `pkg/selector/{filter,score}` 包内的 map —— 入侵式，第三方无法在自己仓库注册；
2. `ScorePluginConf` 是**固定字段结构体**，每加一个 score 插件就要改 config 结构体。
   现成的反面例证：`config.go:632` 已定义 `TemplateScore *TemplateScore` 字段并可被 yaml 解析，
   但 `score/init.go:56` 的注册表中**并没有对应的构造函数**——配置面存在、实现面缺失的死字段。
   配置结构与插件集合分离正是 §4.4 改用 per-plugin `args` 的直接动因；
3. 没有"策略 Profile"概念：切换一套策略要同时改 enable_filters / enable_scorers / 多个
   plugin_conf / resource_weights，无法一键切换、也无法按场景组合；
4. reflect 调用无编译期检查（`filter/init.go:32`、`score/init.go:39`），插件名拼写错误时
   `!fn.IsValid()` 直接 `continue` **静默跳过**（`filter/init.go:34`、`score/init.go:41`），
   实际生效策略与配置声明不一致却无任何告警；
5. 构造函数在配置缺失时直接 `panic`，且**并非个例——4 个 score 插件全部如此**：
   `realtimescore.go:28`、`affinityscore.go:24`、`imagescore.go:38`、`multifactorscore.go:24`。
   §4.2.1 要求工厂返回 error 而非 panic，需一并整改这 4 处。

### 2.3 调度质量指标现状

- 仅有零散运行指标：`pkg/scheduler/local.go:239` `reportStdevTrace()` 每 10s 用
  `rcrowley/go-metrics` 计算节点 CPU/内存/MvmNum 使用率**标准差**（`local.go:259-261`）
  并通过 CubeLog 上报；没有装箱率、模板命中率、调度成功率、创建延迟分位数等统一指标定义。
- 没有针对调度器本身的 benchmark；`examples/cube-bench` 是**端到端**沙箱创建/删除延迟
  压测工具（打真实 CubeAPI），无法脱离集群复现"调度决策质量"实验。

---

## 3. 目标与验收标准映射

| # | 验收标准 | 设计方案对应章节 |
|---|---|---|
| 1 | 指标定义文档，benchmark 输出 ≥5 项调度质量指标 | §4.1 指标定义 + §6 benchmark 报告 |
| 2 | 插件统一接入流水线，配置文件可切换策略 Profile | §4.2 插件 API/Registry + §4.3 独立装配 + §4.4 Profile 编译 |
| 3 | ≥3 种内置调度策略，各有适用场景说明与配置示例 | §5.1 ~ 5.3 |
| 4 | 用户自定义插件开发示例与文档 | §5.4 |
| 5 | benchmark 含 ≥3 种 workload，一键运行生成对比报告 | §6.2 |
| 6 | 新策略至少一项指标明显改善，trade-off 需说明 | §6.3 报告 + §9 验收阈值 |
| 7 | 单测覆盖新增插件核心逻辑，PR 过代码规范检查 | §7、§8 |

---

## 4. 总体设计

### 4.0 设计思路、架构与重点技术

本方案优先解决四个问题：插件仓库与上游仓库解耦、热路径性能不退化、配置可复现、
接口升级可验证。这里的“插件不用动”应准确理解为：**上游正常更新不会造成插件源码的
Git 合并冲突；在 `pluginapi/v1` 兼容范围内插件源码无需修改，只需要用目标上游版本重新
构建并通过兼容测试**。任何项目都不能在依赖语义发生破坏性变化时承诺二进制永远兼容。

对成熟项目的实现调研如下：

| 项目 | 采用方式 | 可借鉴点 | 本项目结论 |
|---|---|---|---|
| Kubernetes Scheduler Framework | 扩展点 + Profile；插件静态编译进自定义 scheduler，通过 `WithPlugin` 显式注入 | 扩展点清晰、配置与代码解耦、out-of-tree 插件不改上游源码 | 采用显式注入、Profile、版本兼容矩阵 |
| Caddy / xcaddy | 模块注册 + 构建器生成临时 Go module，将第三方模块静态编译进二进制 | 用户插件独立仓库、版本可锁定、发行过程可自动化 | 二期提供 `cubemaster-builder`，不要求用户维护上游 fork |
| Volcano Scheduler | Session 生命周期；每轮调度读取一致的 Cluster Snapshot | 避免一次调度中节点数据前后不一致，生命周期边界明确 | 采用 CycleState/快照以及 Start/Close 生命周期 |
| HashiCorp go-plugin / Terraform | 外部进程 + RPC/gRPC + 协议版本 | 隔离崩溃、支持独立升级 | 不用于逐节点 Filter/Score 热路径；RPC 开销和运维复杂度过高 |
| Go `plugin` | 运行时加载 `.so` | 无需重编主程序 | 不采用：工具链、依赖、构建参数需严格一致，平台支持有限 |

最终选择：**版本化插件 SDK + 实例级 Registry + 组合根显式注入 + out-of-tree 自定义发行版
以及静态编译**。这不是简单地把静态 map 换成全局 map：全局 `init()` 注册只能消除 reflect，
仍存在隐式依赖、测试相互污染和接入文件冲突；显式注入才真正把插件所有权移出上游源码。

总体架构：

```text
独立插件仓库                         CubeSandbox 上游仓库
┌──────────────────┐              ┌──────────────────────────────────────┐
│ my-filter        │ 依赖稳定 v1  │ scheduler-plugin-sdk/v1              │
│ my-score         ├─────────────►│ pluginapi/v1: Factory/Snapshot/Status│
└────────┬─────────┘              └────────────────┬─────────────────────┘
         │ WithSchedulerPlugin                     │ adapter
         ▼                                         ▼
┌──────────────────┐              ┌──────────────────────────────────────┐
│ custom main 或   │─────────────►│ Registry → Profile Compiler         │
│ cubemaster-builder│ 显式装配      │ → Pipeline Runtime (immutable)      │
└────────┬─────────┘              └────────────────┬─────────────────────┘
         │ 静态编译                                 │ 每次调度创建 CycleState
         ▼                                         ▼
┌────────────────────────────────────────────────────────────────────────┐
│ PreFilter → Filter → Score → PostScore → Pick → Metrics/Trace         │
└────────────────────────────────────────────────────────────────────────┘
```

关键边界：SDK 只暴露稳定、只读的请求与节点快照；scheduler 内部的 `localcache`、
`node.Node`、全局配置和可写方法均不属于插件 API。旧 `filter.Selector` / `score.Selector`
暂时保留，通过 adapter 接入同一 Pipeline，完成迁移后再弃用。

重点技术包括：用版本化 import path 固化外部契约；用依赖注入取代全局 `init()`；用
ProfileCompiler 将 YAML 编译成不可变 Pipeline；用 CycleState 保证单轮一致读；用 typed
Status 统一失败/Backoff 语义；用虚拟事件、确定性 RNG 和共享事件流保证 benchmark 可复现。

### 4.1 调度评估指标体系（交付物 1）

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

### 4.2 版本化插件 API 与实例级 Registry（交付物 2）

#### 4.2.1 `pluginapi/v1`

第一版即建立小而稳定的 API，避免让仓库外插件直接依赖 `selctx.SelectorCtx` 和
`node.Node` 等内部类型：

```go
package v1

type Plugin interface { Name() string }

type FilterPlugin interface {
    Plugin
    Filter(ctx context.Context, state CycleState, node NodeSnapshot) Status
}

type ScorePlugin interface {
    Plugin
    Score(ctx context.Context, state CycleState, node NodeSnapshot) (int64, Status)
}

type Factory func(
    ctx context.Context,
    args json.RawMessage,
    handle Handle,
) (Plugin, error)
```

`NodeSnapshot`、`RequestSnapshot` 只含调度必需的不可变值；`Handle` 只提供稳定服务，
如 logger、metrics、clock 和只读的镜像状态查询。工厂在启动期完成参数解析和校验，配置
错误必须返回 error 并阻止启动，不允许 `panic`，也不创建 `Disable()=true` 的半有效实例。

`Status` 使用有限类型而不是依赖错误字符串：`Success`、`Unschedulable`、
`UnschedulableAndUnresolvable`、`Error`。硬约束插件返回 `Unresolvable` 后框架不执行
Backoff，从框架层替代当前针对 Template 的 `shouldSkipBackoffForTemplate` 特判。

`pluginapi/v1` 发布后只允许增加新的独立可选接口或字段能力，不给已有接口增加方法；
破坏性变更进入新的 import path `pluginapi/v2`。SDK 与 CubeMaster release 的支持范围由
兼容矩阵声明并在 CI 中实际编译、运行契约测试。

#### 4.2.2 实例级 Registry

```go
type Registry struct {
    factories map[string]pluginapi.Factory
    frozen    bool
}

func NewRegistry() *Registry
func (r *Registry) Register(name string, f pluginapi.Factory) error
func (r *Registry) Freeze()
func (r *Registry) New(ctx context.Context, name string,
    args json.RawMessage, h pluginapi.Handle) (pluginapi.Plugin, error)
```

- Registry 隶属于一个 `App` 实例，不使用 package-global map，也不依赖 `init()` 顺序；
- 空名称、nil factory、重复注册、冻结后注册都返回明确错误；应用启动时统一失败；
- `Names()` 返回排序结果，便于诊断和生成 `--list-scheduler-plugins`；
- 内置插件由 `DefaultRegistry()` 显式注册；单测可创建干净 Registry，互不污染；
- 配置引用未知插件、插件类型与扩展点不匹配时 fail fast，不能告警后跳过，否则实际
  调度策略会与配置声明不一致。

现有 Selector 由 `legacyFilterAdapter` / `legacyScoreAdapter` 包装，迁移阶段保持默认配置
行为不变；新插件只面向 v1 API 开发，不把临时兼容接口继续扩散为公共契约。

### 4.3 组合入口、独立插件仓库与构建器

#### 4.3.1 显式组合入口

将 flag、配置初始化和 server 启动收敛到可复用入口，并使用 Option 注入插件：

```go
// CubeMaster/cmd/cubemaster/app
func Main(opts ...Option)

func WithSchedulerPlugin(name string, factory pluginapi.Factory) Option

// 上游默认二进制
func main() { app.Main() }
```

外部插件仓库自行提供约十行的发行入口，不修改 CubeSandbox 的 `main.go` 或 selector 源码：

```go
package main

import (
    "company.example/cube-plugins/myfilter"
    "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemaster/app"
)

func main() {
    app.Main(app.WithSchedulerPlugin(myfilter.Name, myfilter.New))
}
```

推荐目录：

```text
cube-plugins/                       # 独立 Git 仓库
├── go.mod                          # 固定 CubeSandbox 与 pluginapi 版本
├── cmd/cubemaster-custom/main.go   # 组合根
├── plugins/myfilter/
├── compatibility.yaml              # 已验证的 core/SDK 版本矩阵
└── .github/workflows/compat.yaml   # build + contract test
```

这回答了“写在项目下面，后续更新会不会冲突”：如果插件目录和 blank-import 挂载文件直接
放在上游工作树里，Git pull/rebase **仍可能冲突**；改成独立仓库和自定义入口后，上游升级
只是修改 `go.mod` 版本并重新构建，不会发生源码合并冲突。编译或契约测试失败时才需要按
明确的 API 变化升级插件。

#### 4.3.2 `cubemaster-builder`（二期）

借鉴 xcaddy，提供可复现构建器：

```bash
cubemaster-builder build \
  --core github.com/tencentcloud/CubeSandbox@v0.6.0 \
  --with company.example/cube-plugins/myfilter@v1.2.0
```

构建器创建临时 Go module、生成显式 Option 装配的 main、执行 `go mod verify` 和测试后
输出静态二进制/镜像及 `build-manifest.json`（core、SDK、插件版本与源码校验和）。它降低
发行门槛，但不参与运行时；第一期先交付自定义 main 模板，不让构建器阻塞核心能力。

不选择方案的原因：

- 不用 Go `.so` 动态加载：其工具链与依赖一致性要求比“重新静态构建”更难运维；
- 不用 RPC sidecar 执行每节点打分：节点数 × 插件数会放大序列化、网络与故障处理成本；
- 不承诺运行时热插拔：调度策略是控制面关键路径，静态插件 + 启动校验更可预测。

### 4.4 Profile 编译与不可变运行时配置

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
            # 因子名沿用 constants.go:221-240 的既有取值，避免与 legacy 配置产生两套命名
            factors: { template_id: 1 }
        - name: real_time_weighted_average
          weight: 1
          args:
            factors: { realtime_create_num: 3, quota_cpu_usage: 1, quota_mem_usage: 1 }
      picker: { mode: best }
```

> **因子命名必须与现有常量一致**（`pkg/base/constants/constants.go:221-240`，
> 消费点 `pkg/selector/score/utils.go:18-53`）：`realtime_create_num`（非
> `real_time_create_num`）、`quota_cpu_usage` / `quota_mem_usage`（非 `quota_cpu` / `quota_mem`）、
> `mvm_num`、`cpu_util`、`image_id`、`template_id`。legacy translator 依赖这套名字做双向映射，
> 若在新 API 中改名则属于破坏性变更，须走 §4.2.1 的 v2 流程，本方案不改名。

与 legacy 的结构差异：现状 `EnableWeightFactors []string`（`config.go:644-648`）只声明启用哪些
因子，权重统一从全局 `Score.ResourceWeights map[string]float64`（`config.go:68`）查
（`utils.go:65-76` `getFactorWeight`）——即**因子的启用与权重被拆在两处、且权重全局共享，
不同 scorer 无法对同一因子取不同权重**。Profile 的 `args.factors` 把二者合并为 per-plugin
的 `map[string]float64`，legacy translator 负责把旧的 `[]string` + 全局 map 翻译成等价的
per-plugin map，保证默认行为不变。

启动时由 `ProfileCompiler` 完成 schema 校验、插件查找、args 解码、权重范围检查和实例化，
输出不可变 `RuntimeConfig`。Pipeline 只读取该快照，不在调度热路径读取全局配置。

兼容策略：未配置 `profiles` 时，通过 legacy translator 把现有
`enable_filters/enable_scorers/plugin_conf` 编译成 `legacy-default` Profile；迁移两个
release 后再评估弃用。第一版 Profile 变更明确要求重启 CubeMaster。现有配置热更新与插件
实例生命周期并不一致，若只热更权重会形成“旧实例 + 新配置”的混合状态；后续若确有需要，
再以“构建新 Pipeline → 原子切换 → drain 旧 Pipeline → Close”的方式实现整组热切换。

Profile 的 picker 支持：`best`、`top_n_uniform`、`top_n_weighted`。

`legacy-default` Profile 的 picker **必须由 `least_select_name` 与 `priority_select_num`
两个配置项联合翻译**，不能简单固定为某一种（§2.1）：

| legacy 配置 | 等价 picker |
|---|---|
| `least_select_name: random` + `priority_select_num: -1`（**缺省组合**） | `top_n_uniform` with N = 全部候选 |
| `least_select_name: random` + `priority_select_num: N` | `top_n_uniform` with N |
| `least_select_name: rw`/`sw`/`rrw` + `priority_select_num: N` | `top_n_weighted` with N |

翻译错误会悄悄改变结果分布，因此 §7 的 legacy golden test 必须覆盖上表全部组合。

### 4.5 CycleState、生命周期与并发模型

每次调度创建一个 `CycleState`，包含 RequestSnapshot、候选节点快照、Profile generation
以及同一轮插件共享的只读/局部状态。所有插件在一次调度中观察到同一版本的数据，避免
Filter 和 Score 分别读取 localcache 时出现时间撕裂。

可选生命周期接口：

```go
type Validator interface { Validate() error }
type Starter interface { Start(context.Context) error }
type Closer interface { Close() error }
```

框架顺序固定为 `New → Validate → Start → Serve → Close`。异步预计算 scorer 在 `Start`
中启动 goroutine，在 `Close` 中退出；错误由 context 取消和健康指标反馈，不允许插件自行
管理无界后台任务。Filter/Score 必须支持并发调用，框架在文档中明确线程安全契约。

`CycleState` 解决读取一致性，不等于资源预占。#695 所述并发超卖仍需独立的原子
reservation/rollback 机制；benchmark 同时模拟有无 reservation，并将 oversell 单独报告，
不能把策略得分改善误写为并发正确性修复。

### 4.6 调度链路埋点

- Pipeline 出入口用 defer 记录 attempts 和 duration，失败原因映射到有限枚举；
- Pick 后根据同一 CycleState 判断模板命中，避免再次读缓存造成口径漂移；
- 创建链路记录真正的端到端 create counter/histogram；Profile generation 写入日志/trace，
  不作为 Prometheus label；
- 10s 周期任务从同一节点快照计算 quota、CV、active nodes 和 fragmentation；
- 热路径只做无界 label 禁止的 Counter/Histogram 观测，不记录 node_id/template_id，
  防止 Prometheus 时序爆炸。

---

## 5. 内置调度策略（交付物 3）

三个策略均是可版本化 Profile；尽量组合现有插件，只在缺少能力时新增插件。

### 5.1 `burst-spread`：突发短生命周期沙箱

- **场景**：AI Agent 并发拉起大量秒级至分钟级沙箱，优先避免单节点创建风暴；
- **Filter**：`cpu`、`mem`、`disk`、`realtime_create_num`；不使用模板硬亲和；
- **Score**：复用已有 `real_time_weighted_average`，提高 `realtime_create_num`、
  `mvm_num`、`cpu_util` 权重（因子名见 §4.4 表格）。原方案的 `spread_score` 与现有实时打分
  公式重复，不新增；
- **Picker**：`top_n_weighted`，N=3~5，在保持质量的同时减少同分热点；
- **权衡**：预期负载 CV 下降，但模板本地命中率和缓存复用可能下降。

### 5.2 `template-hotstart`：同模板高频热启动

- **场景**：RL 训练、批量评测等重复创建同一模板，优先缓存命中和启动延迟；
- **Filter**：默认只保留资源、磁盘与并发硬约束，不用 `template_locality` 硬过滤；
- **Score**：提高现有 `image_score` 权重，并用 `real_time_weighted_average` 在副本节点间均衡。
  原方案再增加 `template_affinity_score` 会与 `image_score` 重复，因此删除；
- **Picker**：`best`，使高权重本地性得分稳定生效；
- **严格模式**：可选 `template-hotstart-strict` Profile 加入 locality 硬 Filter。它可能获得
  接近 100% 的命中率，但资源不足时会降低成功率，必须单独报告，不作为默认值。

### 5.3 `binpack`：大规格长驻沙箱

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

### 5.4 自定义插件开发示例（交付物 4）

新增中英文 `scheduler-plugin-development.md`，提供一个独立可构建的
`examples/scheduler-plugin/`：

- `label_filter` 展示 v1 Filter、args 解码、typed Status 与表驱动测试；
- `zone_spread_score` 展示 Score、CycleState、指标和确定性打分；
- `cmd/cubemaster-custom` 展示 `WithSchedulerPlugin` 显式装配，不修改上游文件；
- `go.mod` 固定 core/SDK 版本，CI 对最近两个 CubeSandbox release 与 `master` 执行 build、
  race test 和最小契约测试；
- 文档说明 API 稳定面、版本矩阵、升级步骤、线程安全和生命周期要求。

---

## 6. 可复现调度 Benchmark / 仿真器（交付物 5、6）

### 6.1 形态与可信度边界

新增 `CubeMaster/cmd/schedbench/`，将生产调度主流程提取为可注入依赖的 `Pipeline`。
生产与 benchmark 共用完整的 PreFilter/Filter/Score/PostScore/Pick 代码，差别仅在
NodeProvider、ImageProvider、Clock、RNG 和资源账本的实现。

仿真器采用虚拟事件时间，不 `sleep`，事件依次为：

```text
Arrival → Schedule → Admission/Reservation → CreateComplete
                                           → LifetimeExpire → Release
```

- 同一 workload 事件流在所有 Profile 间复用；随机源由 `--seeds` 指定，默认跑固定 seed 集；
- picker 的同分节点使用 NodeID 稳定破平，不依赖 map 遍历顺序；
- `--reservation=optimistic|none` 对比原子预占与当前竞态窗口，输出 oversell 次数；
- 每份报告记录 Git commit、Profile 内容哈希、workload 参数、seed、Go 版本；
- 离线工具只能报告调度耗时与**建模启动成本**，不得把模型值命名为真实“创建 P95”；
  真实端到端创建延迟通过可选 cluster 模式复用 `examples/cube-bench` 测量。

一键入口：`make sched-bench`，输出 Markdown、JSON 和 CSV。

### 6.2 三种内置 Workload

| Workload | 默认参数 | 模拟目标 |
|---|---|---|
| `burst` | 500 请求、泊松到达 50/s、寿命 U(10s,120s)、统一小规格 | 高并发短生命周期 |
| `template_storm` | 300 请求、同一 TemplateID、30% 节点预置副本 | 重复模板与缓存热点 |
| `mixed_spec` | 400 请求、1C2G:2C4G:8C16G=6:3:1、混合寿命 | 大小规格组合与资源碎片 |

实验矩阵 = workload × profile（legacy-default / burst-spread / template-hotstart / binpack）
× seed；节点快照、请求序列和 overcommit 对照组完全一致。

### 6.3 报告指标与调优建议

报告至少输出：调度成功率、P50/P95 调度耗时、模板命中率、CPU/Mem CV、活跃节点数、
空节点数、CPU/Mem 碎片率、重调度率、oversell 次数以及模拟成本。关键表格同时给出绝对值、
相对 baseline 变化、seed 间均值和离散程度，避免用单个随机种子下结论。

自动建议仅基于显式规则生成，例如 CV 过高时提示增加实时余量权重、模板命中低且成功率
有余量时提示增加 `image_score` 权重。报告必须同时列出负向变化，不输出“全面优于默认”
这类无法由多目标调度实验支持的结论。

---

## 7. 测试与质量保障

| 层级 | 覆盖点 |
|---|---|
| SDK 契约测试 | v1 DTO/Status 行为、Factory 错误传播、接口兼容样例可编译 |
| Registry 单测 | 实例隔离、重复/冻结注册、排序 Names、未知插件、扩展点类型错误 |
| Profile 编译测试 | legacy 翻译、args schema、权重边界、未知名 fail fast、快照不可变 |
| Pipeline 单测 | 各扩展点顺序、typed Status/Backoff、取消、panic recovery、并发安全 |
| 策略单测 | 打分单调性、零配额、极端规格、dominant resource、稳定破平 |
| 指标单测 | counter 标签、CV 边界、利用率/碎片率公式、模板命中口径 |
| Benchmark 回归 | 固定事件流结果确定、账本最终释放、reservation 模式可检出 oversell |
| 兼容矩阵 | 插件示例对最近两个 release 和 master build/test；记录允许失败的开发分支 |

新增代码执行 `go test -race ./...`（资源允许的 package）、`go vet ./...`、
`golangci-lint run ./...` 和 `gofmt`。基准测试设置性能门槛：默认 Profile 在固定 100 节点
用例中，调度吞吐下降不超过 5%，P95 额外开销不超过 5%；超限需 profile/pprof 解释。

---

## 8. 时间规划与阶段交付

按 6 周安排，每周形成可独立审查、可回滚的 PR，避免最后一次性提交大改动：

| 时间 | 重点工作 | 交付物与退出条件 |
|---|---|---|
| 第 1 周 | 指标口径、Pipeline 依赖抽取、设计评审 | 本方案定稿；指标文档；legacy 行为 golden test 通过 |
| 第 2 周 | `pluginapi/v1`、实例 Registry、legacy adapter | SDK/Registry PR；契约与 race test 通过；默认行为不变 |
| 第 3 周 | `app.Main(opts...)`、外部示例、ProfileCompiler | 独立插件仓库模板可构建；legacy 配置可翻译；错误配置启动失败 |
| 第 4 周 | 三个 Profile、demand-aware binpack、Prometheus 埋点 | 策略/指标 PR；单测和性能门槛通过 |
| 第 5 周 | schedbench、三类 workload、reservation 对照 | 一键生成 MD/JSON/CSV；固定 seed 可复现；输出 ≥5 指标 |
| 第 6 周 | 参数实验、文档、兼容矩阵、上游反馈修订 | 对比报告；中英文开发文档；全部 CI 通过；准备提交 PR |

推荐 PR 顺序：Pipeline/指标基础 → SDK/Registry → 显式入口/Profile → 策略 → benchmark →
文档与示例。`cubemaster-builder` 是二期增强，不进入六周关键路径。

PR 提交注意（遵循 `CONTRIBUTING.md` 与根目录 `AGENTS.md`）：

- commit message 使用英文 Conventional Commits 风格；
- **AI 不得添加 `Signed-off-by`**，由人类提交者自行签署 DCO；AI 辅助需在 commit/PR 中
  标注 `Assisted-by: <AGENT_NAME>:<MODEL_VERSION>`；
- PR 描述附设计文档、benchmark 原始配置和对比报告，方便复核结论。

---

## 9. 预期效果、验收阈值与风险

以下是验收目标，不是脱离实验的性能承诺；最终以相同 workload、seed 和 overcommit 的
对照报告为准，至少一个新策略达到对应主目标且守住成功率门槛：

| Profile | 主目标（相对 legacy-default） | 守门指标 | 已知权衡 |
|---|---|---|---|
| `burst-spread` | `burst` CPU 或 Mem CV 相对下降 ≥10% | 成功率下降 ≤1 个百分点 | 模板命中可能降低 |
| `template-hotstart` | 命中率提升 ≥10 个百分点；cluster 模式同时观察创建 P95 | 成功率下降 ≤1 个百分点 | 热点节点负载升高 |
| `binpack` | `mixed_spec` 活跃节点数或碎片率相对下降 ≥10% | 成功率下降 ≤1 个百分点 | CV 与故障影响面升高 |

工程效果：外部插件不修改上游工作树；Profile 切换由一份配置完成；错误插件和错误参数在
启动期暴露；benchmark 可复现实验且报告不少于五项质量指标；新增插件有完整示例和兼容矩阵。

主要风险及应对：

- **API 设计过宽**：v1 只暴露不可变 DTO 和最小 Handle；新增能力走独立可选接口；
- **静态编译仍需重建**：这是换取热路径性能和部署确定性的明确权衡，builder 降低操作成本；
- **Profile 热更新预期**：v1 文档明确 restart-required，避免半更新状态；
- **指标基数与性能**：禁止请求、节点、模板 ID 作为 label；通过 benchmark 守住 5% 门槛；
- **调度与资源竞态混淆**：单列 reservation/oversell 指标，#695 作为并发正确性后续项；
- **策略不存在全局最优**：每个 Profile 明确适用 workload 与负向指标，报告展示完整 trade-off。

---

## 10. 调研依据

- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)：扩展点、调度周期与插件模型；
- [Kubernetes Scheduler Configuration](https://kubernetes.io/docs/reference/scheduling/config/)：多 Profile 与插件配置；
- [Kubernetes Scheduling Framework KEP](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/624-scheduling-framework/README.md)：out-of-tree 插件和 `WithPlugin`；
- [Kubernetes scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins)：自定义 scheduler 入口与版本兼容矩阵；
- [Caddy Extending Caddy](https://caddyserver.com/docs/extending-caddy) 与 [xcaddy](https://github.com/caddyserver/xcaddy)：外部模块静态组合和构建器；
- [Volcano Scheduler Configuration](https://github.com/volcano-sh/volcano/blob/master/docs/user-guide/how_to_configure_scheduler.md)：插件 Session 生命周期；
- [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin)：外部进程插件、协议版本与隔离边界；
- [Go plugin package](https://pkg.go.dev/plugin)：动态加载的工具链、依赖和平台限制。

---

## 附：关键代码位置速查

| 内容 | 位置 |
|---|---|
| 调度主流程 `Select()` | `CubeMaster/pkg/scheduler/schedule.go:28` |
| 并行 Filter / 评分 / PostScore 调用 | `schedule.go:157` / `:201` / `:251` |
| 调度器全局单例与初始化 | `CubeMaster/pkg/scheduler/init.go:33`（包级变量）、`:43` `InitScheduler` |
| `Select`/`BackoffSelect` 全部调用点 | `sandbox_run.go:474`、`sandbox_migrate.go:40`（仅此 2 处） |
| Filter 接口与静态注册 | `CubeMaster/pkg/selector/filter/init.go:17,32,43` |
| Score 接口与静态注册 | `CubeMaster/pkg/selector/score/init.go:19,39,56` |
| 调度上下文 | `CubeMaster/pkg/scheduler/selctx/selectcontext.go:21`、`:151` `LeastRandomSelect` |
| 选择器权重被忽略之处 | `CubeMaster/pkg/scheduler/selctx/random_select.go:21-23` |
| 调度配置结构体 | `CubeMaster/pkg/base/config/config.go:241`（SchedulerConf）、`:627`（ScorePluginConf） |
| 选择相关缺省值 | `config.go:248-249`（字段）、`config.go:1203-1208`（缺省填充） |
| overcommit 默认值与生效逻辑 | `config.go:339-340`（默认值）、`config.go:348` `GetEffectiveOvercommitRatio` |
| `EffectiveAllocated` 归零陷阱 | `config.go:441`（受 `ignore_redis_allocation` 影响，§4.1 观测口径依据） |
| 权重因子常量与消费点 | `pkg/base/constants/constants.go:221-240`、`pkg/selector/score/utils.go:18-53,65-76` |
| 现有负载均衡度上报（stddev） | `CubeMaster/pkg/scheduler/local.go:239,259-261` |
| 模板本地性 Filter（阶段内重读缓存） | `pkg/selector/filter/template_locality.go:50,91` |
| 镜像/模板 Score | `pkg/selector/score/imagescore.go:128-129`（template_id 因子）、`:160-168`（打分二值化） |
| score 构造函数 panic（需整改 4 处） | `realtimescore.go:28`、`affinityscore.go:24`、`imagescore.go:38`、`multifactorscore.go:24` |
| 现有 Prometheus 链路 | `CubeMaster/go.mod:30`、`pkg/server/server.go:107`（`/metrics`）、`pkg/templatecenter/snapshot_metrics.go:13-32` |
| 端到端压测工具（cluster 模式可复用） | `examples/cube-bench/` |
| 新增 Filter 插件的既有范例 | 上游 PR #1157（SRA filter） |
