# 调度策略真实控制面 A/B 基准报告

> 分支：`feat/scheduler-eval-plugin`（含调度插件系统、场景 Profile、schedsim、cube-bench 改造）
> 日期：2026-09-07 ｜ 环境：单台 96C/246G 开发机上的真实控制面 + 模拟数据面

> **⚠ 时效说明（2026-09-09）。** 下文 A/B 数据测量于 **`selection.method: spread`
> 修复之前**（修复见 `93f2f457`）：当时 `spread` 是静默 no-op、等价于 `random`
> （即本报告问题清单第 1 条），三个场景 Profile 都没有真正执行其声明的选择语义；
> 且 legacy 侧以零 scorer 编译，退化为近似 first-fit 堆叠（第 3 条）。阅读下文时请把
> "legacy" 理解为*无打分的 first-fit*，把各 Profile 的放置理解为*有打分但 top_n 内
> 近似随机*。修复后的复测结论见文末
> [spread 修复后的复测（2026-09-09）](#spread-修复后的复测-2026-09-09)，完整数据见
> [调度仿真评测报告](./scheduler-sim-report)。

本文记录对调度策略 Profile 的一次**真实控制面** A/B 实测：除数据面（Cubelet）为模拟外，全部组件为真实进程——真实编译的 CubeMaster / CubeAPI（Rust）二进制、MariaDB、Redis，以及真实的 cube-bench 压测流量。本报告是验收标准 6（默认策略 vs 新策略的量化对比）的实证材料。

## 测试拓扑与保真度

```
cube-bench ──HTTP──▶ CubeAPI(:3000) ──HTTP──▶ CubeMaster(:8089) ──gRPC──▶ fakecubelet ×50
                     真实 Rust 二进制          真实调度流水线/Profile        (127.0.1.1–50:9999)
                     MariaDB 10.3 + Redis（节点库存 DB，节点指标走 Redis 心跳，同生产路径）
```

模拟数据面行为模型：**模板命中 50ms 响应 / 未命中 800ms 并随后缓存升温**，按节点配额记账并周期上报 Redis。50 节点 × 64C/128G，模板预载比例 30%（seed 42 确定性抽取）。

保真度限制（重要）：fake 的负载指标（`cpu_load_usage` 等）不随沙箱数增长，因此 `node_safety` 永远不会触发限流——legacy 策略"238 个沙箱堆一台节点"的现象在真实集群中不可能发生。绝对延迟数字不代表生产环境，**结论只在 locality vs spread 的方向性上成立**。

## 方法

三个 workload（cube-bench 预设）各跑 legacy（无 profiles 配置）与对应场景 Profile 两个变体，共 6 组：burst ↔ burst_balance、template_storm ↔ template_reuse、mixed_spec ↔ mixed_binpack。同一 workload 两个变体使用**字节级相同**的请求 trace（seed 42），节点舰队、预载抽取完全一致——唯一变量是调度配置。并发 `-c` 按 `速率×寿命` 预留（700–900），queue delay P50 ≈ 0.6ms，客户端无自我限流。Profile 生效由指标标签 `profile="burst_balance"` 等确认（legacy 为 `profile="default"`）。

## 结果

创建延迟（客户端测量；miss = 延迟 >400ms，与 fake 的 cache_misses 计数完全吻合）：

| workload / 变体 | n | P50 | P95 | 平均 | miss% | 成功率 |
|---|---|---|---|---|---|---|
| burst / legacy | 500 | 58.4ms | 810.0ms | **212.0ms** | **20.4%** | 100% |
| burst / burst_balance | 500 | 63.4ms | 814.2ms | **391.1ms (+84%)** | **44.2%** | 100% |
| template_storm / legacy | 300 | 59.7ms | 815.9ms | **265.6ms** | **27.3%** | 100% |
| template_storm / template_reuse | 300 | 60.5ms | 815.1ms | **357.1ms (+34%)** | **39.7%** | 100% |
| mixed_spec / legacy | 400 | 58.4ms | 812.1ms | **194.2ms** | **18.0%** | 100% |
| mixed_spec / mixed_binpack | 400 | 61.5ms | 817.2ms | **360.9ms (+86%)** | **40.0%** | 100% |

放置分布（运行期每 5s 从 Redis 探测 `mvm_num`，为地面真值）：

| 组 | 峰值涉及节点数 | 单节点最多沙箱 |
|---|---|---|
| burst：legacy vs burst_balance | **3 vs 17** | 238 vs 58 |
| template_storm：legacy vs template_reuse | **4 vs 17** | 144 vs 35 |
| mixed_spec：legacy vs mixed_binpack | **9 vs 37** | 59 vs 23 |

服务端交叉验证：`sandbox_create_duration_seconds` 均值与客户端一致（如 burst 209.8 vs 389.3ms）；调度决策本身仅 0.2–0.3ms、1.0 次尝试/创建、零重调度——**全部差异来自放置结果**。

## 结论（反直觉，但方向稳定）

**三个新策略在本实验中全部比 legacy 更慢**，机制一致：它们都把放置摊得更开，而每新触及一台冷节点就要付一次 800ms 冷启动（并发创建同一冷节点时还有惊群叠加）。legacy 的"无脑堆叠"意外地对模板缓存温度最优。

- **template_reuse**：调度侧的 locality 命中决策确实提升（`template_hit="true"` 占比 18.0%→28.7% storm / 20.3%→30.3% mixed），但因为预载副本集覆盖 34/50 节点，"偏好副本节点"反而把放置摊开到每模板约 27–30 台节点，真实缓存 miss 从 82 升到 119——**决策命中率升了，真实延迟却差了 34%**。
- **mixed_binpack**：名义上装箱，实际是**最分散**的变体（37/50 节点）。`resource_fit_score` 打分方向确实是装箱（剩余越少分越高），但 (1) `selection.method: spread` 是 no-op（见下），top_n=2 之间近似随机选；(2) 3 倍 overcommit 下当前占用率处配额差异太小，分数聚拢；(3) 权重 0.3 的 `real_time_weighted_average`（均衡向）反向拉扯。最坏处：mixed_spec 大规格 tpl-c（8C16G）P50 = **808ms**（legacy 为 60ms），55% 大规格创建走了冷节点。
- **burst_balance**：放置确实更均衡（峰值 3→17 节点），但延迟 +84%。

## 实验暴露的代码问题（已列入 review）

1. **`selection.method: spread` 是 no-op，等价于 `random`**：`CubeMaster/pkg/scheduler/schedule.go:164-172` 只特判 `highest`，其余都走 `LeastRandomSelect(TopN)`。三个内置场景 Profile 全部使用 `spread`——**没有任何一个跑过其宣称的语义**。本次实验的反直觉结果很大程度源于此。
2. **缺少 `scheduler.score.plugin_conf.<scorer>` 时 Profile 编译在启动期 panic**（`imagescore.go:37-38`、`realtimescore.go:27-28`、`multifactorscore.go:23-24`），而非给出配置校验错误；sim 示例 yaml 也未写明 `score:` 块是必需的。
3. **legacy 默认流水线在无 `score:` 配置时编译出零 scorer**，配合 `priority_select_num=1` 退化为近似确定性 first-fit，导致极端堆叠（238/节点）。

## spread 修复后的复测（2026-09-09）

`spread` 选择语义已修复（`schedule.go` `spreadSelect`），sim 基线已带 least-loaded
打分，本矩阵在 schedsim 中复跑（相同 workload 预设与 trace 参数，300 个模拟节点，
30% 模板预置 + 远程 restore 升温模型，seed 42–46，quality + performance 两种模式）。
完整数据见[调度仿真评测报告](./scheduler-sim-report)。对上文物结论的修正要点：

| 对照（300 节点，5 seed 均值） | 旧结论（spread 失效时） | 修复后 sim 结果 |
| --- | --- | --- |
| burst：legacy vs burst_balance | 新策略"摊得更开，延迟 +84%" | 相对*带打分的* legacy 放置中性（Jain 均 0.567、命中率均 0.594、成功率 1.0）。相对真正无打分的 first-fit legacy，摊平才是主要收益：Jain 0.418→0.567、羊群度 1.3%→0.4%、CV −21%。旧 legacy 的优势来自零 scorer 堆叠，不是真实策略效果。 |
| template_storm：legacy vs template_reuse | "决策命中率升了，真实延迟却差 34%" | 修复后决策质量没有歧义：template_hit_rate = 1.000，legacy 0.324 / first-fit 0.573——locality 打分完全达到设计目标。代价是负载集中在副本节点上（Jain 0.243、活跃节点 78/300），这是本地化与均衡的预期取舍。旧报告的延迟回退是否在真实数据面仍存在，取决于 restore 成本，需要在真机上复测。 |
| mixed_spec：legacy vs mixed_binpack | "名义装箱，实际最分散（37/50 节点）" | 有了真 spread 语义后装箱器确实装箱：活跃节点 7.26/300（legacy 165.5），template_hit 0.956，成功率 1.0，碎片率 0（集群分配率仅 ~2.3%）。代价：羊群度 16%、决策 P99 +63%（1.56→2.54 ms）。旧报告"装箱打分不敏感"的观察是 spread no-op 的假象，不是 `resource_fit_score` 的问题。 |

框架开销的直接测量（performance 模式，分阶段墙钟计时）：Profile 流水线新增强制
guards 阶段（P50 约 0.09ms），但省去了 legacy 的可选 filter 阶段（约 0.06ms）；
决策总成本净增 +3%…+13%（P50），各阶段中 prefilter 候选枚举始终占大头（约 65%）。
4 vCPU 机器上调度核心吞吐约 1.0–1.4k 次决策/秒/进程，P99 全程个位数毫秒。

## 后续实验建议

- ~~实现真正的 `spread` 语义（或改用 `method: highest`）后复跑本矩阵~~ —— **已完成**（`93f2f457`），sim 复测见上。真机集群上复跑同一矩阵仍待做：延迟轴需要真机验证（fake 的 800ms 冷启动模型与放置宽度存在耦合）；
- fake 负载指标与占用率联动（使 `node_safety` 生效）后复跑，观察 legacy 堆叠行为被约束后的对比；
- 每组 3 次重复以量化惊群方差（本次同配置复跑 miss 数在 52–102 间波动，方向稳定）。

## 复现

测试工具链（fakecubelet、编排脚本、配置）为一次性实验代码，未合入仓库。关键参数：50 节点 × 64C/128G，模板预载 30%，fake 延迟模型 50ms 命中 / 800ms 未命中（升温），cube-bench `-c 700–900`，seed 42。如需复跑请联系本报告作者获取 `/root/realtest/` 下的 harness。
