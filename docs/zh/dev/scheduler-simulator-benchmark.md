# 调度 Simulator 基准

本文说明如何用最小离线基准评估 CubeMaster 调度 Profile，而无需真实多节点集群。

## 范围

simulator 刻意保持精简。它只建模 CubeMaster 调度器的高层语义：

1. 拒绝放不下请求的节点；
2. 对可行候选打分；
3. 把沙箱放到最高分节点；
4. 记录放置与调度质量指标。

它不能替代线上的 `cubebox multirun` 基准，也不宣称生产吞吐或延迟。

## 一键运行

在 `CubeMaster` 目录下：

```bash
go run ./cmd/schedulerbench --out ./schedulerbench-report
```

命令会写出：

- `schedulerbench-report/report.json`
- `schedulerbench-report/report.md`

可用 `--format json`、`--format markdown` 或 `--format both` 选择输出文件集合；默认是 `both`。

默认运行是确定性的，并在两份报告中记录 seed、节点数、workload、profile、指标 schema 与验收映射。`--nodes` 仅接受 **1–4**；CLI 显式 `--nodes 0` 会被拒绝，而不会静默改成默认值。

使用 `--verify` 可在写文件前，对照课题一默认验收合同检查内存中的报告：

```bash
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```

校验要求：文档约定的默认 workload 与 profile 已跑完整矩阵；关键指标 schema 与估算延迟标记存在；每个非 default profile 在每个 workload 上都有 default-vs-candidate 对比；报告用语须标明延迟是 simulator 估算而非实测。

该检查仅覆盖 simulator。它不会部署真实多节点环境，不会经 CubeAPI/Cubelet 创建资源，也不会验证生产性能或真实创建延迟。

## Workload

- `burst_short_lived`：80 个短生命周期小请求，在短时间内突发到达。
- `same_template_repeated`：48 个重复使用同一模板的请求，用于暴露模板本地性。
- `mixed_size`：60 个混合小/中/大规格请求，用于暴露容量与装箱 trade-off。

## Profiles

这些名称是离线基准使用的 **仅限 simulator** 打分权重预设。Profile 名称字符串有意与 Draft PR #1666 中另行跟踪的集成工作对齐，以便相关改动保持命名一致。本 PR **不**交付运行时 `scheduler.profile` overlay，也不暗示对该 draft 工作存在独立运行时依赖。

- `default`：资源余量与打散打分均衡，并带轻度模板本地性。
- `balanced_spread`：更倾向在节点间均匀放置。
- `template_locality_first`：更倾向已缓存所请求模板的节点。
- `binpack_utilization`：更倾向更紧的装箱，并显式权衡均衡与余量。

## 指标

报告包含超过五项调度质量指标：

- `schedule_success_rate`
- `rejected_requests`
- `node_load_balance`
- `template_locality_hit_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `create_latency_p50_ms`：估算创建延迟 P50（毫秒）。这是 simulator 估算值，不是 CubeAPI/Cubelet 创建耗时。无成功样本时为 `0`。
- `create_latency_p95_ms`：估算创建延迟 P95（毫秒），估算器与空样本规则同 P50。
- `uses_estimated_latency`：在本 simulator 中恒为 `true`，避免读者把 P50/P95 当成实测创建延迟。
- `peak_cpu_utilization`
- `average_cpu_headroom`
- `average_score_margin`：所选节点与第二名候选的分数差均值，仅对至少有两个已打分候选的成功调度请求取平均；无此类观测时为 `0`。
- `average_candidates_scored`：每个成功调度请求平均考虑的截断后候选数。过滤与打分后受 simulator 优先级候选上限约束；不是内部检查过的全部可行节点数。

Markdown 结果表包含 `latency p50 ms` 与 `latency p95 ms`。估算器是确定性的：

```text
estimated_latency_ms =
    80
  + (template already on node ? 0 : 120)
  + (active_sandbox_count / max_sandboxes) * 80
  + ((cpu_pressure + mem_pressure) / 2) * 60
```

只有成功调度的请求贡献样本。P50/P95 是对这些样本做线性插值得到的分位数。

## 对比

JSON 报告包含 `comparisons` 数组。每项在同一 workload 上把 `default` profile 与某个非 default profile 对比。

差值恒为 `candidate - baseline`。延迟指标越低越好，因此负的延迟差值表示改善。

分类口径偏保守：

- 比率类指标至少需要 `0.005` 的绝对变化；估算延迟至少需要 `1.0` ms。
- `schedule_success_rate` 一旦下降，就不能标为 `improved`。
- `regressed`：成功率下降且没有其它改善，或只有重要指标变差。
- `trade_off`：成功率下降但部分指标改善，或部分指标改善同时其它重要指标变差。
- `improved`：至少一个关键指标改善，且参与对比的指标没有明显变差。
- `neutral`：参与对比的指标都未越过小变化阈值。

Markdown 报告有 `## Comparisons` 表，含 workload、candidate、result 以及主要改善/回退指标。Notes 限定为“本次模拟 workload 下观察到”，不是生产断言。

参与对比的 delta 键：

- `schedule_success_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `node_load_balance`
- `template_locality_hit_rate`
- `create_latency_p50_ms`
- `create_latency_p95_ms`

## 验收映射

- 验收路径：每个 workload 对每个 profile 跑一次，并报告 baseline-vs-profile 的放置与质量指标。
- 领域视角：先过滤不可行节点，再对可行候选打分，最后绑定最高分；遵循与 CubeMaster selector 相同的 filter → score → bind *形态*，使用 simulator 本地打分权重（并非生产 Filter/Score 插件等价）。
- 失败路径：非法 profile/workload 名称会使运行失败；不可行请求计入拒绝并带明确原因。
- 证据路径：JSON 与 Markdown 报告包含 seed、节点数、workload 定义、profile 名、放置计数与指标 schema。
- 审阅路径：simulator 独立放在 `pkg/scheduler/simulator`，CLI 是 `cmd/schedulerbench` 下的薄封装；不改动生产调度默认值。
- 差异化角度：报告内建测量路径与结论证据上下文，而不只展示标题数字。

## 验证

```bash
go test ./pkg/scheduler/simulator
go run ./cmd/schedulerbench --out ./schedulerbench-report
```
