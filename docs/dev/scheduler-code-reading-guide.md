# CubeSandbox 调度系统代码阅读指南

> 配套文档：[scheduler-plugin-design.md](./scheduler-plugin-design.md)（设计/修复方案）
> 目标读者：准备实现"集群调度性能评估与高性能调度插件系统"任务的同学
> 代码基线：`master @ c458228e`（本 PR 与 master 的 merge base）

本文按**推荐阅读顺序**列出实现该任务需要读的代码，标注每个文件"为什么读、读什么、
关键位置"。标注 📌 的为必读，其余可按需选读。

---

## 第 0 阶段：建立整体认识（半小时）

先读文档再读代码，知道调度器在系统中的位置：

- 📌 仓库根 `README.md` 的架构图：CubeAPI → CubeMaster → Cubelet 三段式控制面。
- 📌 `docs/architecture/`（及 `docs/zh/architecture/`）：调度器是 CubeMaster 的子模块。
- 设计文档 [scheduler-plugin-design.md](./scheduler-plugin-design.md) §2：本文与之对应，读代码时随时对照。

---

## 第 1 阶段：调度主链路（核心中的核心，约 3500 行）

调度代码全部在 `CubeMaster/pkg/scheduler/` 与 `CubeMaster/pkg/selector/` 两个目录，
**总量不大，建议通读**。

### 1.1 📌 `CubeMaster/pkg/scheduler/schedule.go`（242 行，全读）

调度主流程 `Select()`（`Select` 函数定义处）。必须弄清：

- 流水线顺序：`runPreFilter` → `runFilter`（`parallelRunFilters`，L138，**并行执行各
  Filter 后取交集**）→ `runScoreFilter`（L178，各插件得分 × 权重求和、归一化）→
  `postScore` → `LeastRandomSelect(PrioritySelectNum)`。默认 `random` selector 在得分前 N 名
  中均匀随机，传入的权重会被忽略；`priority_select_num=1` 时选择最高分节点。
- 失败路径：Filter 失败 → `BackoffSelect`（L72）；`shouldSkipBackoffForTemplate`
  （L59）——模板创建请求**不走** backoff，这是模板本地性策略的关键分支。
- 你将在这里插入：`Select()` 出入口的延迟/成功率埋点、模板命中判定。

### 1.2 📌 `CubeMaster/pkg/scheduler/init.go`（42 行，全读）

`InitScheduler()`：preSelector / backoffSelector / filter / score / postScore 的装配点。
Profile 机制落地时，这里（或下游 `NewSelector()`）是切换策略组合的入口。

### 1.3 📌 `CubeMaster/pkg/scheduler/selctx/selectcontext.go`（175 行，全读）

调度上下文 `SelectorCtx`：贯穿整条流水线的数据载体。重点：

- `RequestResource`（L39）：一次创建请求的资源需求（Cpu/Mem/磁盘/`TemplateID`/
  `ErofsImages`/`TemplateNodeScope`）——你的 Filter/Score 插件能拿到的全部请求信息。
- `Affinity`（L33）：节点亲和性（NodeSelector / 优选 terms）。
- `LeastRandomSelect`（L134）：构造最终 Top-N 候选及权重；实际选择语义由
  `least_select_name` 指定的 selector 决定，默认 `random` 为均匀随机。
- `lastBadFilters` / `FilterOut`：熔断（circuit filter）机制。

### 1.4 📌 `CubeMaster/pkg/scheduler/local.go`（319 行，全读）

调度器的后台任务，**和"评估指标"直接相关**：

- `reportStdevTrace()`（L217）：现有的节点 CPU/内存/MvmNum 使用率**标准差**上报
  （`rcrowley/go-metrics` + CubeLog Trace）——你的装箱率/均衡度指标要复用这条链路。
- `monitorLimit()`（L259）：按健康节点数动态调整每节点创建并发上限
  （buffer queue 限流）——理解"突发短生命周期"场景必须先理解这段。
- `AddBufferTask`（L110）：创建任务的缓冲队列入口。

### 1.5 📌 调用点：创建与迁移路径

- `CubeMaster/pkg/service/sandbox/sandbox_run.go`：`createSandboxContext.schedule()`
  （L467）调用 `scheduler.Select`；`errorCodeRetry()`（L428）与 `backoffRetryDelay()`
  （L483）——调度失败/cubelet 失败后的重试与重调度逻辑，"重调度率"指标埋在这里；
  `dealMetric()`（L499）——创建链路现有的 metric 上报方式，可仿照。
- `CubeMaster/pkg/service/sandbox/sandbox_migrate.go` L40：迁移也走 `Select`，
  指标采集别忘了这条路径。

---

## 第 2 阶段：插件接口与内置插件（你的插件要对齐的范式）

### 2.1 📌 接口定义（两个文件，各 50 行左右）

- `CubeMaster/pkg/selector/filter/init.go`：`Selector` 接口（`Select + ID`，L16）、
  静态注册 map `filters`（L40）、`NewSelector()` 的 reflect 实例化（L22）。
- `CubeMaster/pkg/selector/score/init.go`：`Selector` 接口（`Select + ID + Weight +
  Disable`，L18）、静态注册 map `scores`（L52）。

**这两个 map 就是现有的代码内静态注册点**。新方案不会只把它们替换成全局 Registry，
而是通过 `pluginapi/v1`、实例级 Registry 和启动入口显式注入完成迁移，详见设计文档 §4.2。

### 2.2 📌 内置 Filter 插件（6 个，每个 60~90 行，全读）

`CubeMaster/pkg/selector/filter/`：

| 文件 | 干什么 | 阅读重点 |
|---|---|---|
| `cpufilter.go` | CPU 配额+利用率硬过滤 | `EffectiveQuotaCpu`/`EffectiveAllocated` 的用法（overcommit 在这里生效） |
| `memfilter.go` | 内存配额过滤 | 同上 |
| `diskfilter.go` | 磁盘使用率过滤 | `DiskUsageMaxPercent` |
| `template_locality.go` | 模板本地性硬过滤 | `localcache.GetImageStateByNode` 判定本地副本；`TemplateNodeScope` |
| `realtimecreatelimit.go` | 单节点创建并发硬过滤 | `RealTimeCreateConcurrentLimit` 与 `LocalCreateConcurrentLimit × HealthyMasterNodes` 的双层限流 |
| `thirtpartyfilter.go` | 第三方实例类型过滤 | 了解即可 |

### 2.3 📌 内置 Score 插件（4 个 + 工具）

`CubeMaster/pkg/selector/score/`：

- 📌 `realtimescore.go`：`real_time_weighted_average` —— 实时配额余量加权打分，
  **新打分插件的最佳模板**（注意 `NewRealTimeWeightedAverageScore` 构造函数 panic 的问题，注册表改造要顺手解决）。
- 📌 `utils.go`：**全部 14 个打分因子函数**（`getMvmNumScore`、`getQuotaCpuUsageScore`、
  `getCpuUtilScore`…），写 spread/binpack 打分时直接复用或反用（binpack 就是
  "利用率越高分越高"，与现有因子方向相反）。
- 📌 `imagescore.go`：`image_score` —— 镜像/模板本地性打分，`template-hotstart`
  策略的核心插件，注意 `EnableWeightFactors` 里 `image_id` / `template_id` 两个因子。
- `multifactorscore.go` + `asyncscore.go`：`multi_factor_weighted_average` ——
  后台按 `ScoreInterval` 周期预计算 `n.Score`（`loopAsyncScore` 的 50ms ticker
  只做轮询，到点由 `checkDeadline` 放行重算），调度时直接读。
  **性能敏感场景的参考**：把重计算挪出调度热路径。
- `affinityscore.go`：节点亲和性优选打分（配合第 3 阶段的 affinity 读）。

### 2.4 📌 其余三段流水线

- `prefilter/prefilter.go`：健康/熔断/亲和/MvmNum 上限/指标新鲜度，
  产出候选节点全集（`localcache.GetSchedulableNodesByInstanceType`）。
- `postscore/whilelistscore.go`：白名单加减分（运营干预手段）。
- `backofffilter/backofffilter.go`：Filter 全灭后的降级路径（只查亲和/磁盘/MvmNum）。

### 2.5 📌 `CubeMaster/pkg/scheduler/affinity/nodeaffinity.go`（155 行）

K8s 风格 NodeSelector（In/NotIn/Exists/Gt/Lt）。亲和相关的策略（如专属集群、
大规格机型亲和 `LargeSizeAffinityConf`）依赖它。

---

## 第 3 阶段：配置体系（Profile 机制落在哪）

- 📌 `CubeMaster/pkg/base/config/config.go`：
  - `SchedulerConf`（L178）：调度器全部配置字段——overcommit、并发数、亲和、
    各类 per-instance-type 覆盖。Profile 字段要加在这里。
  - `SchedulerFilterConf`（L499）/ `SchedulerScoreConf`（L513）/ `ScorePluginConf`
    （L519）/ `PostScoreConf`（L503）：现有的插件启用与权重配置。
    注意 `ScorePluginConf` 是**固定字段结构体**——每加一个 score 插件都要改它，
    这是扩展性短板之一。
  - `GetEffectiveOvercommitRatio`（L273）与默认值（CPU=3、Mem=2，L265）。
  - `EffectiveQuotaCpu` / `EffectiveAllocated` 等（L490 附近）：配额换算公共函数。
- 📌 `CubeMaster/conf.yaml` L85 起的 `scheduler:` 段：配置的实际形态，
  你的 Profile 示例要加在这里。
- 📌 `CubeMaster/pkg/base/constants/constants.go`：`SelectorFilterID` 等 ID 前缀
  （L9-13）与 20 个 `WeightFactor*` 因子名（L241-260）。

---

## 第 4 阶段：数据底座（调度决策依赖的数据从哪来）

- 📌 `CubeMaster/pkg/base/node/node.go`：`Node` 结构体（L20）——配额
  （`QuotaCpu/QuotaMem`）、已分配（`QuotaCpuUsage/QuotaMemUsage`）、实时利用率
  （`CpuUtil/CpuLoadUsage/MemUsage`）、磁盘、MvmNum、健康状态等，打分因子的原料。
- 📌 `CubeMaster/pkg/localcache/export.go`：调度器的数据访问门面，重点函数：
  `GetSchedulableNodesByInstanceType`（L118）、`GetHealthyNodes`（L107）、
  `MaxMvmLimit`/`RealMaxMvmLimit`（L352/372）、`CreateConcurrentLimit`/
  `RealTimeCreateConcurrentLimit`（L383/401）、`HealthyMasterNodes`（L454）、
  `GetImageStateByNode`（L463）。
- 选读：`localcache/node_cache.go`（节点清单维护）、`image_cache.go` /
  `template_locality.go`（模板副本分布——"模板命中率"指标的数据源）、
  `redis_cache.go`（分配账本在 Redis 的记录，`ignore_redis_allocation` 开关的由来）。
- 选读：指标上报链路——节点指标如何进入 localcache（Cubelet `/v1/metrics/scheduler`，
  背景见上游 issue #342/#326）。仿真器要 mock 的就是这一层的数据快照。

---

## 第 5 阶段：测试与工具（照着写，别发明新轮子）

- 📌 现有单测（作为新插件测试模板）：
  - `selector/filter/template_locality_test.go`、`selector/score/imagescore_test.go`
    （494 行，表驱动 + fake 数据的完整范例）；
  - `scheduler/schedule_test.go`（操作包级 `scheduler.filter` 变量的手法）；
  - `selector/score/asyncscore_test.go`、`selctx/selectcontext_test.go`。
- 📌 `examples/cube-bench/`：现有端到端压测工具（打真实 CubeAPI，测创建/删除延迟，
  有 `--dry-run`、JSON 导出、报表 UI）。你的 `schedbench` 仿真器可以复用它的
  报表/统计代码（`stats.go`、`report.go`），但注意它**不含调度决策质量**维度。
- 选读：`tests/e2e/`——E2E 框架，验收时若需要真实集群回归可看这里。

---

## 第 6 阶段：工程规范（提交前必读）

- 📌 根目录 `AGENTS.md`：**AI 不得加 `Signed-off-by`**（DCO 由人类签署）；
  AI 辅助的 commit/PR 必须带 `Assisted-by: <AGENT>:<MODEL>`。
- 📌 `CONTRIBUTING.md`：构建（`make cubemaster`）、文档 PR 规范
  （docs 中英目录、kebab-case 文件名、frontmatter）。
- 📌 `CubeMaster/.golangci.yaml`：代码规范检查配置——验收标准里"PR 通过项目代码
  规范检查"指的就是它，提交前本地跑 `golangci-lint run`。
- `CubeMaster/Makefile`：测试/构建入口。

---

## 第 7 阶段：外部参考（选读）

- 上游 PR #1157（`cubemaster: add scarce-resource scheduler filter (SRA v1)`）：
  **一个"新增 Filter 插件"从 issue 到合入的完整范例**，照着它的 PR 结构与单测写。
- 上游 issue #695（Concurrent CreateSandbox scheduling race）：理解并发创建超卖问题，
  benchmark 的"突发并发"workload 就是冲这个来的。
- Kubernetes Scheduler Framework（PostFilter/Score/Reserve 概念）：本项目的
  PreFilter/Filter/Score/PostScore 是它的简化版，概念对照有助于写文档。

---

## 读完自测清单

读完上述 📌 部分后，你应该能不看代码回答：

1. 一个创建请求从 `sandbox_run.go` 到选中节点的完整路径？Filter 全失败时发生什么？
2. 新增一个 Filter / Score 插件，今天需要改哪几个文件？（答案：插件文件本身 +
   `init.go` 的 map + `ScorePluginConf` 结构体 + conf.yaml —— 这就是要改造的 4 个点）
3. `enable_scorers` 里的插件权重与 `resource_weights` 里的因子权重如何叠加？
   （`runScoreFilter` L194：`n.Score *= f.Weight()`，因子权重在各插件内部先加权）
4. overcommit 在哪里生效？（`EffectiveQuotaCpu/Mem`，被 cpu/mem filter 与打分因子共用）
5. 模板本地性如何保证？它的"命中率"指标该用什么数据计算？
   （`template_locality` 硬过滤 + `image_score` 软打分；`GetImageStateByNode`）
6. 现有均衡度指标（stddev）在哪计算、如何上报？你的 5 项新指标如何接入同一链路？
7. 并发创建限流的两层机制（buffer queue + realtime_create_num filter）分别在哪？

答不上来的，回对应阶段重读。

---

## 工作量参考

| 阶段 | 内容 | 大约阅读量 | 建议耗时 |
|---|---|---|---|
| 0 | 文档/架构 | — | 0.5h |
| 1 | 调度主链路 | ~1300 行 | 2~3h |
| 2 | 插件接口与内置插件 | ~1400 行 | 3~4h |
| 3 | 配置体系 | ~300 行（定位式阅读） | 1h |
| 4 | 数据底座 | ~500 行（定位式阅读） | 1~2h |
| 5 | 测试与工具 | ~800 行 | 1~2h |
| 6 | 工程规范 | — | 0.5h |

之后即可按 [scheduler-plugin-design.md](./scheduler-plugin-design.md) §8 的 PR 划分动工。
