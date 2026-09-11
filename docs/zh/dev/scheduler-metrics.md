# CubeSandbox 调度评估指标

本文定义课题一调度质量指标，并说明它们如何对应当前 scheduler simulator 报告。指标用于相对比较：在同一节点集、同一 workload、同一随机种子下，对比 `default` 与候选 Profile 的差异。

这些数字是离线调度决策证据，不是生产集群性能。simulator 不启动真实 MicroVM，也不测量 CubeAPI 创建耗时。

实现来源：

- 指标计算：`CubeMaster/pkg/scheduler/simulator/simulator.go` 中的 `Metrics`
- 报告输出：在 `CubeMaster` 目录执行 `go run ./cmd/schedulerbench`
- 运行方式：`docs/dev/scheduler-simulator-benchmark.md`

当前 CLI（`CubeMaster/cmd/schedulerbench/main.go`）标志：

| 标志 | 默认值 | 含义 |
|---|---|---|
| `--out` | `schedulerbench-report` | 写入 `report.json` 和 `report.md` 的目录 |
| `--seed` | `20260903`（`DefaultConfig().Seed`） | 记入报告的确定性种子。CLI 显式传入 `--seed 0` 为非法（库侧零值 `Config{}` 仍默认到 `20260903`）。当前 seed 只影响 `burst_short_lived` 的模板序列；`same_template_repeated` 与 `mixed_size` 使用固定请求集。 |
| `--nodes` | `4`（`DefaultConfig().NodeCount`） | 模拟节点数；支持范围为 **1–4**。CLI 显式传入 `--nodes 0` 为非法（库侧零值 `Config{}` 仍默认到 4）。 |
| `--profiles` | `default,balanced_spread,template_locality_first,binpack_utilization` | 逗号分隔的 Profile 列表 |
| `--workloads` | `burst_short_lived,same_template_repeated,mixed_size` | 逗号分隔的 workload 列表 |
| `--format` | `both` | 输出格式：`json`、`markdown` 或 `both` |
| `--verify` | `false` | 写报告前检查默认报告的结构与内部一致性；不验证实时性能 |

常用命令：

```bash
go test ./pkg/scheduler/simulator ./cmd/schedulerbench
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```

未知 `--profiles` / `--workloads` 名称会使 `simulator.Run` 返回错误，CLI 以非零退出。
`--verify` 只验证离线 simulator 报告合同，不运行真实多节点环境，也不验证
CubeAPI/Cubelet 的真实创建延迟或生产性能。

## 指标总览

| 指标 | JSON 字段 | 单位 | 方向 | 课题验收 |
|---|---|---|---|---|
| 调度成功率 | `schedule_success_rate` | ratio `[0,1]` | 越高越好 | 调度成功率 |
| CPU 配额利用率 | `cpu_quota_utilization` | ratio `[0,1]` | 越高越好 | 集群装箱率 |
| 内存配额利用率 | `mem_quota_utilization` | ratio `[0,1]` | 越高越好 | 集群装箱率 |
| 节点负载均衡度 | `node_load_balance` | ratio `[0,1]` | 越高越好 | 负载均衡 |
| 模板本地命中率 | `template_locality_hit_rate` | ratio `[0,1]` | 越高越好 | 模板本地性 |
| 创建延迟 P50 | `create_latency_p50_ms` | ms | 越低越好 | 创建延迟 |
| 创建延迟 P95 | `create_latency_p95_ms` | ms | 越低越好 | 创建延迟 |

课题原文要求至少覆盖装箱率、负载均衡度、模板命中率、调度成功率和创建延迟 P50/P95。当前 simulator 报告会输出以上全部字段，并在每条 `metrics` 中带 `uses_estimated_latency`（恒为 `true`）。完整 `Metrics` / `NodeLoad` 字段见下方映射表。

## 测量口径

所有核心指标都在一次 workload 跑完后写入 `results[].workloads[].metrics`。需要先固定以下口径，再解释单个指标。

1. 请求按 `Arrival` 升序处理；同一 tick 内先释放 `EndsAt <= tick` 的 sandbox，再调度该 tick 到达的请求。
2. 节点不可行条件遵循本 simulator 建模的硬资源形态（sandbox 数达到上限，或 CPU/内存配额放不下本次请求）。这是带 simulator 本地约束的 Filter→Score→bind *形态*，并不宣称与 CubeMaster 生产 Filter 插件集合等价。不可行节点不进入 Score。
3. 可行节点按 Profile 权重打分后，绑定最高分节点（确定性 argmax，同分按节点 ID 稳定打破）。生产 CubeMaster 会按 `scheduler.priority_select_num` 截断（发行配置为 `1`），再按 `scheduler.least_select_name` 在截断集合内选择——默认 `random` 为均匀随机，`sw`/`rw`/`rrw` 为按分数加权。发行配置 `priority_select_num: 1` 时截断集合只有一个节点，因此生产选择与本 simulator 的 argmax 一致（同分打破规则可能不同）。
4. `average_score_margin` 是上述 argmax 绑定下的 simulator 本地决策差距代理，不是生产选择指标。
5. 配额利用率、峰值利用率和负载均衡度使用**最后一次请求到达后的节点快照**，不是全时段平均，也不是历史峰值。
6. 模板命中率和估算延迟只统计成功调度的请求。
7. 节点初始模板缓存是静态的。成功放置不会把模板加入该节点缓存。

因此，短生命周期 workload 结束时节点上可能只剩尚未到期的请求。装箱率和均衡度反映的是这次快照，而不是高峰占用。

## 当前报告字段映射

当前 CLI 写出两份同构报告：

- `schedulerbench-report/report.json`
- `schedulerbench-report/report.md`

JSON 路径如下。

```text
report.json
  provenance                       git_revision / git_dirty
  metric_schema[]                  指标合同：name / description / direction
  results[]
    profile
    workloads[]
      workload
      metrics                      下表中的字段都在这里
```

Markdown 结果表列名与 JSON 字段对应关系（`Report.Markdown()`）：

| Markdown 列 | JSON 字段 |
|---|---|
| `profile` | `results[].profile` |
| `workload` | `results[].workloads[].workload` |
| `success` | `schedule_success_rate` |
| `rejected` | `rejected_requests` |
| `locality hit` | `template_locality_hit_rate` |
| `load balance` | `node_load_balance` |
| `cpu util` | `cpu_quota_utilization` |
| `mem util` | `mem_quota_utilization` |
| `latency p50 ms` | `create_latency_p50_ms` |
| `latency p95 ms` | `create_latency_p95_ms` |
| `avg feasible` | `average_feasible_candidates` |

`Metrics` 的 JSON 字段与 `simulator.go` 中 struct tag **一一对应**，没有其它 metrics 键：

| Go 字段 | JSON 字段 | 用途 |
|---|---|---|
| `TotalRequests` | `total_requests` | 成功率分母 |
| `ScheduledRequests` | `scheduled_requests` | 成功率分子，也是命中率和延迟样本分母 |
| `RejectedRequests` | `rejected_requests` | 无可行节点的请求数 |
| `SuccessRate` | `schedule_success_rate` | `scheduled_requests / total_requests` |
| `PlacementCounts` | `placement_counts` | 各节点成功放置次数 |
| `NodeLoadBalance` | `node_load_balance` | 最终快照节点负载均衡度 |
| `TemplateLocalityHitRate` | `template_locality_hit_rate` | 成功请求落在初始热模板节点的比例 |
| `AverageCPUUtilization` | `cpu_quota_utilization` | 最终快照节点 CPU 利用率算术平均 |
| `PeakCPUUtilization` | `peak_cpu_utilization` | 最终快照中节点 CPU 利用率最大值 |
| `AverageMemUtilization` | `mem_quota_utilization` | 最终快照节点内存利用率算术平均 |
| `PeakMemUtilization` | `peak_mem_utilization` | 最终快照中节点内存利用率最大值 |
| `CreateLatencyP50MS` | `create_latency_p50_ms` | 成功请求估算创建延迟 P50 |
| `CreateLatencyP95MS` | `create_latency_p95_ms` | 成功请求估算创建延迟 P95 |
| `UsesEstimatedLatency` | `uses_estimated_latency` | simulator 中恒为 `true` |
| `AverageCPUHeadroom` | `average_cpu_headroom` | 每次成功放置后的平均 CPU 余量 |
| `AverageScoreMargin` | `average_score_margin` | 第一名与第二名分数差的均值，仅对至少有两个已打分候选的调度决策取平均；无此类观测时为 **0** |
| `AverageFeasibleCandidates` | `average_feasible_candidates` | `feasible_candidate_evaluations / scheduled_requests` |
| `FeasibleEvaluations` | `feasible_candidate_evaluations` | 成功调度请求上的可行节点数之和 |
| `FailureReasons` | `failure_reasons` | 有拒绝时出现；当前只记 `no_feasible_node`（`omitempty`） |
| `Warnings` | `warnings` | 有拒绝时出现容量提示（`omitempty`） |
| `NodeFinalState` | `node_final_state` | 各节点最终占用；值为 `NodeLoad` |

`node_final_state.<id>` 对应 `NodeLoad`：

| Go 字段 | JSON 字段 |
|---|---|
| `RunningSandboxCount` | `running_sandbox_count` |
| `UsedCPUMilli` | `used_cpu_milli` |
| `UsedMemMB` | `used_mem_mb` |
| `CPUUtilization` | `cpu_utilization` |
| `MemUtilization` | `mem_utilization` |

`scoreCandidates` 对全部可行节点打分排序，并绑定最高分节点（确定性 argmax）。
生产 CubeMaster 会按 `scheduler.priority_select_num` 截断（发行配置为 `1`），再按
`scheduler.least_select_name` 在截断集合内选择——默认 `random` 为均匀随机，
`sw`/`rw`/`rrw` 为按分数加权。发行配置 `priority_select_num: 1` 时生产选择与本
argmax 一致（同分打破规则可能不同）。在本 simulator 的绑定规则下，全量排序后再
截断不会改变最终绑定节点，所以报告只暴露
`feasible_candidate_evaluations` / `average_feasible_candidates` 作为
simulator 本地候选宽度代理（最大为节点数）。

`average_score_margin` 同样是 argmax 绑定下的 simulator 本地决策差距代理，
不是生产选择指标。

`docs/dev/scheduler-benchmark-report-schema.md` 描述当前 CLI 输出的 profile × workload 矩阵和 `comparisons[]` 对比结构。本文以当前 `Metrics` 字段为准。

## 调度成功率

字段：`schedule_success_rate`

### 定义

workload 中成功选出节点并完成模拟绑定的请求比例。

### 计算方式

```text
schedule_success_rate = scheduled_requests / total_requests
```

对应代码：`Metrics.SuccessRate = ratio(ScheduledRequests, TotalRequests)`。

一次请求计为成功，当且仅当至少有一个节点满足：

```text
len(active) < MaxSandboxes
used_cpu + request.cpu <= node.cpu_quota
used_mem + request.mem <= node.mem_quota
```

否则计入 `rejected_requests`，并在 `failure_reasons["no_feasible_node"]` 加一。当前 simulator 不区分 CPU 不足、内存不足或 sandbox 数上限；这三类都归入同一失败原因。

`scheduled_requests + rejected_requests` 必须等于 `total_requests`。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `mixed_size` | 主观察项。大规格请求更容易无可落点，成功率能暴露装箱与碎片差异。 |
| `burst_short_lived` | 次观察项。默认 4 节点下当前种子常为 1.0，此时不能单独证明策略更好。 |
| `same_template_repeated` | 次观察项。请求规格均匀，成功率通常接近 1.0，更适合看命中率和均衡度。 |

### 能证明什么

- 在该节点容量和请求序列下，策略是否把请求放到了可行节点上。
- 候选策略是否以牺牲可调度性换取装箱、均衡或本地性。
- 失败是否来自容量不足：看 `rejected_requests` 和 `failure_reasons`。

### 不能证明什么

- 不能证明真实集群的 API 成功率、Cubelet 创建成功或沙箱就绪。
- 不能证明 Filter 插件组合的完整语义。simulator 只保留资源/数量硬约束，没有模板本地性硬过滤，也没有实时创建数过滤。
- 成功率为 1.0 不能证明调度质量高，只说明这次请求没有超出模拟容量。

## CPU 配额利用率

字段：`cpu_quota_utilization`

### 定义

workload 结束后，各节点 CPU 配额利用率的算术平均。这里的占用是已绑定且尚未到期的 sandbox 配额，不是主机真实 CPU 使用率。

### 计算方式

对最终快照中的每个节点：

```text
cpu_utilization_i = used_cpu_milli_i / node_cpu_milli_i
cpu_quota_utilization = mean(cpu_utilization_i)
```

对应代码：`cpuUtilization(nodes)` 的第一个返回值，写入 `Metrics.AverageCPUUtilization`。

注意：

- 这是节点等权平均，不是集群加权 `sum(used_cpu) / sum(node_cpu)`。当前默认节点 CPU 配额不同（4000/4000/6000/8000），两种算法结果会不同。
- `used_cpu` 只包含最终快照仍 active 的请求。已到期释放的短任务不计入。
- 同快照的节点值在 `node_final_state.<id>.cpu_utilization`。
- `peak_cpu_utilization` 是该快照上的节点最大值，不是运行过程中的历史峰值。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `mixed_size` | 主观察项。规格差异大，最能体现 `binpack_utilization` 的装箱目标。 |
| `same_template_repeated` | 次观察项。生命周期较长，最终快照仍能留下可比较的占用。 |
| `burst_short_lived` | 不适合单独作为装箱证据。生命周期短，结束时大量请求已释放，最终利用率会低估高峰装箱。 |

### 能证明什么

- 策略是否倾向于把剩余请求留在更满或更空的节点上。
- 在最终仍有占用的 workload 上，候选策略相对 baseline 的配额装箱差异。
- 结合 `peak_cpu_utilization` 和 `node_final_state`，可以看出占用是分散还是集中在个别节点。

### 不能证明什么

- 不能证明真实 CPU busy、steal、cgroup 使用率或 overcommit 后的可运行性。
- 不能证明时间平均装箱率或高峰装箱率。
- 利用率升高不一定更好：可能伴随均衡度下降、延迟上升或成功率下降。

## 内存配额利用率

字段：`mem_quota_utilization`

### 定义

workload 结束后，各节点内存配额利用率的算术平均。占用同样是尚未到期的 sandbox 内存配额。

### 计算方式

```text
mem_utilization_i = used_mem_mb_i / node_mem_mb_i
mem_quota_utilization = mean(mem_utilization_i)
```

对应代码：`memUtilization(nodes)` 的第一个返回值，写入 `Metrics.AverageMemUtilization`。内存单位是模拟配额中的 MB，报告不换算成字节。

口径与 CPU 利用率相同：节点等权、最终快照、不含已释放请求。节点明细在 `node_final_state.<id>.mem_utilization`，快照最大值在 `peak_mem_utilization`。

当前默认节点上，部分 workload 的 CPU 与内存请求比例接近，两个利用率可能同向变化。`mixed_size` 含更大内存形状，两者可能分叉；比较装箱时应两个字段一起看。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `mixed_size` | 主观察项。大内存请求更容易制造碎片，内存利用率比 CPU 更敏感。 |
| `same_template_repeated` | 次观察项。 |
| `burst_short_lived` | 不适合单独作为装箱证据，原因与 CPU 利用率相同。 |

### 能证明什么

- 策略在内存配额维度上的最终装箱差异。
- CPU 看起来更满但内存仍空、或相反时，是否存在单维装箱假象。
- 大规格请求被拒绝时，内存利用率是否已经接近节点上限。

### 不能证明什么

- 不能证明真实 RSS、cache、balloon 或主机内存压力。
- 不能证明时间平均或高峰内存装箱率。
- 不能单独证明碎片更少；需要同时看成功率和 `placement_counts`。

## 节点负载均衡度

字段：`node_load_balance`

### 定义

最终快照中，节点综合负载的均衡程度。值越接近 1，节点间负载越接近；越接近 0，负载越倾斜。

### 计算方式

对每个节点：

```text
load_i = 0.5 * cpu_utilization_i + 0.5 * mem_utilization_i
```

然后：

```text
mean = average(load_i)
if mean == 0:
    node_load_balance = 1
else:
    stddev = sqrt(sum((load_i - mean)^2) / n)   # 总体标准差，分母为节点数
    node_load_balance = clamp(1 - stddev / mean, 0, 1)
```

对应代码：`nodeLoadBalance(nodes)`。`stddev / mean` 是变异系数（CV）。所有节点负载为 0 时定义为 1，避免除零。

该指标使用最终快照，因此短任务释放后可能把高峰期的不均衡抹平。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `burst_short_lived` | 主观察项，对应 `balanced_spread`。若最终快照占用已被释放，应同时看 `placement_counts`，不要只看这一个数。 |
| `mixed_size` | 主观察项，用来核对 binpack 是否以均衡换装箱。 |
| `same_template_repeated` | 次观察项。模板集中放置时，均衡度下降是预期 trade-off。 |

### 能证明什么

- 最终 CPU/内存占用在节点间是否更均匀。
- spread 类策略是否相对于 binpack / locality 策略降低了节点倾斜。
- 结合 `placement_counts`，可以核对“打散放置”是否真的发生。

### 不能证明什么

- 不能证明瞬时创建风暴被打散。均衡度只用最终快照的 CPU/内存配额占用，不跟踪每节点并发创建数。
- 不能证明调度过程中每个 tick 都均衡；只证明结束快照。
- 不能证明真实节点 load average 或 CPU 饱和对称。
- 节点容量不等时，等权 CV 也不等于按容量加权的集群均衡。

## 模板本地命中率

字段：`template_locality_hit_rate`

### 定义

成功调度的请求中，落在“开始时已缓存该模板”的节点上的比例。

### 计算方式

每次成功绑定后，若 `node.WarmTemplates[request.template] == true`，命中计数加一：

```text
template_locality_hit_rate = template_local_hits / scheduled_requests
```

对应代码：`Metrics.TemplateLocalityHitRate`。`scheduled_requests == 0` 时保持 0。

当前默认缓存：

| 节点 | 初始模板 |
|---|---|
| `node-a` | `python-agent`, `tiny-shell` |
| `node-b` | `python-agent` |
| `node-c` | `data-notebook` |
| `node-d` | `gpu-build`, `data-notebook` |

命中只看初始缓存，不看本次 run 是否刚在该节点创建过同一模板。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `same_template_repeated` | 主观察项。全部请求使用 `python-agent`，直接检验 `template_locality_first`。 |
| `mixed_size` | 次观察项。模板种类多，能看到本地性与大规格可行集的交互。 |
| `burst_short_lived` | 次观察项。模板在 `tiny-shell` 与 `python-agent` 间切换，命中率变化通常小于同模板场景。 |

### 能证明什么

- 策略是否更常选择初始带有该模板的节点。
- 本地性提高是否伴随延迟估算下降，或伴随均衡度下降。
- 未命中请求是否因为可行集里根本没有热模板节点。

### 不能证明什么

- 不能证明真实模板/镜像已经在 Cubelet 本地，也不能证明热启动真的发生。
- 不能证明运行过程中的缓存预热：simulator 不会在首次放置后把模板标为本地。
- 命中率高不能自动证明 P50/P95 更好；资源压力和节点拥塞仍会抬高估算延迟。
- 分母是成功调度数。如果失败请求本来会命中或错过热节点，该指标不会反映它们。

## 创建延迟 P50 / P95

字段：

- `create_latency_p50_ms`
- `create_latency_p95_ms`
- `uses_estimated_latency`

### 定义

成功调度请求的**估算**创建延迟分布。P50 是中位数，P95 是高尾。两者都不是真实创建耗时。`uses_estimated_latency` 在 `runWorkload` 里恒为 `true`，CLI 会把它写进每条 `metrics`。

### 计算方式

每次成功绑定后采样一次：

```text
estimated_latency_ms =
    80
  + (template_local ? 0 : 120)
  + (active_sandbox_count / max_sandboxes) * 80
  + ((cpu_pressure + mem_pressure) / 2) * 60
```

其中：

- `template_local` 使用初始 `WarmTemplates`。
- 采样发生在请求写入 `active` 之后，因此 `active_sandbox_count` 含当前请求。
- `cpu_pressure = used_cpu / node_cpu_quota`，`mem_pressure = used_mem / node_mem_quota`，其中 `used_cpu` / `used_mem` 来自绑定后的节点占用快照，当前请求只计入一次。`cpuHeadroomAfter` / `memHeadroomAfter` 仍只用于绑定前打分；绑定后延迟估算不再调用它们，避免把当前请求重复计入资源压力。

分位数：

```text
sort(samples)
rank = (p / 100) * (n - 1)
value = linear_interpolate(sorted[floor(rank)], sorted[ceil(rank)])
```

对应代码：`estimatedCreateLatencyMS` 与 `percentile`。无成功样本时 P50/P95 为 0；`uses_estimated_latency` 仍为 `true`。

Markdown 列 `latency p50 ms` / `latency p95 ms` 对应前两个字段。`uses_estimated_latency` 出现在 Markdown 的 Metric Contract，不作为结果表单独一列。

### 适用 workload

| Workload | 阅读方式 |
|---|---|
| `same_template_repeated` | 主观察项。本地性变化应优先反映在 P50。 |
| `burst_short_lived` | 主观察项。拥塞和打散应优先看 P95。 |
| `mixed_size` | 次观察项。大规格请求的资源压力会抬高估算延迟，需和成功率一起看。 |

### 能证明什么

- 在该估算模型下，模板本地性、节点拥塞和资源压力如何改变延迟分布。
- P50 下降通常与命中率提高同向；P95 下降更可能来自避免把请求堆到过热节点。
- 候选策略是否用更高延迟换装箱，或用打散换更低尾延迟。

### 不能证明什么

- 不能证明 CubeAPI、Cubelet、快照/克隆或网络路径上的真实创建延迟。
- 不能证明 P99、吞吐、排队时间或端到端就绪时间。
- 不能跨不同 seed、节点数或估算公式比较绝对值。
- 不能把当前报告中的 P50/P95 读成实测值。simulator 的 `uses_estimated_latency` 恒为 `true`。若将来改用实测 create latency，应继续用同名字段，并把该标记改为 `false`；那不是当前 CLI 的行为。

## Workload 常量

`workloadRequests` 只接受下列常量值；其它名称返回 `unknown workload`。CLI `--workloads` 必须使用右侧字符串。

| Go 常量 | CLI / JSON 值 | 请求数 |
|---|---|---|
| `WorkloadBurstShortLived` | `burst_short_lived` | 80 |
| `WorkloadSameTemplateRepeat` | `same_template_repeated` | 48 |
| `WorkloadMixedSizeCreate` | `mixed_size` | 60 |

Profile 同样只有四个合法名：`default`、`balanced_spread`、`template_locality_first`、`binpack_utilization`（对应 `ProfileDefault` 等常量）。其它名称返回 `unknown profile`。

## Workload 读数指南

| Workload | 主要指标 | 次要指标 | 常见 trade-off |
|---|---|---|---|
| `burst_short_lived` | `node_load_balance`、`schedule_success_rate`、`create_latency_p95_ms` | `template_locality_hit_rate`、`placement_counts` | 打散可能降低模板命中率 |
| `same_template_repeated` | `template_locality_hit_rate`、`create_latency_p50_ms`、`create_latency_p95_ms` | `node_load_balance` | 本地性优先可能集中到少数热节点 |
| `mixed_size` | `cpu_quota_utilization`、`mem_quota_utilization`、`schedule_success_rate` | `node_load_balance`、`rejected_requests` | 装箱可能降低均衡度，或把大规格请求挤出可行集 |

比较规则：只改变 Profile，不改变 workload、seed 和节点容量。不要把不同 workload 的指标直接横向比绝对值。

## 改善判断

结论必须落在同一 workload、同一 seed、同一节点容量上。建议使用以下标签：

- `improved`：目标指标改善，且 `schedule_success_rate` 未下降。
- `trade_off`：目标指标改善，但至少一个重要次级指标变差。
- `neutral`：差异过小，不足以支持明显改善。
- `regressed`：目标指标变差，或成功率明显下降。
- `invalid`：缺字段、请求数为 0，或两次运行不可比。

不要只展示最好的一次运行。当前默认 CLI 每个 profile/workload 只跑一个 seed，结论应写成“本次 workload 下观察到”，而不是“该策略普遍更优”。

## 已知限制

- simulator 使用自己的 filter → score → bind 规则，不是完整 CubeMaster 流水线或生产插件语义。
- 最终快照指标不代表高峰占用；短生命周期场景尤其明显。
- 装箱率是节点等权配额利用率，不是集群加权装箱率，也不是真实资源使用率。
- 模板命中基于静态初始缓存。
- 创建延迟是估算模型，含当前实现的压力计算口径。每条 `metrics` 都带 `uses_estimated_latency: true`。
- 失败原因目前只有 `no_feasible_node`。
- `failure_reasons` 与 `warnings` 使用 `omitempty`，全成功时 JSON 中不出现。
- 单 seed 报告不能支持跨环境推广。

## 相关覆盖范围

本文档覆盖：

- simulator 报告输出的调度质量指标定义（至少五项核心指标）；
- 如何用一条 CLI 命令跑三种默认 workload 并生成报告；
- 如何用同一 workload 下的 baseline vs profile 差值说明改善或 trade-off。

相关文档：

- simulator 用法：`docs/dev/scheduler-simulator-benchmark.md`
- 两两对比报告形状：`docs/dev/scheduler-benchmark-report-schema.md`
