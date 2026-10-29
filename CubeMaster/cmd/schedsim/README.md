# schedsim — 调度仿真器

schedsim 用**模拟节点状态**在单个进程内驱动 CubeMaster 真实的调度核心
`scheduler.Select`，回放 cube-bench 生成的请求 trace，用于在无真实集群的
条件下大规模评估调度质量：装箱率、碎片率、负载均衡、羊群度、模板命中率、
调度延迟。

```
cmd/schedsim/main.go        CLI 入口（flag 编排、逐轮驱动、报告输出）
cmd/schedsim/example.sim.yaml  示例调度配置（本目录测试也用它，改它要跑测试）
pkg/scheduler/sim/          可测逻辑：trace 解析、虚拟时钟事件循环、指标纯函数
```

## 用法

```bash
cd CubeMaster
go build -o /tmp/schedsim ./cmd/schedsim

/tmp/schedsim \
  --trace /path/to/trace.json \        # cube-bench --dump-trace 产物（必需）
  --config cmd/schedsim/example.sim.yaml \  # 被测调度配置（必需）
  --nodes 300 \                        # 模拟节点数（同构）
  --node-cpu-millis 64000 \            # 单节点 CPU 配额（毫核）
  --node-mem-mib 131072 \              # 单节点内存配额（MiB）
  --instance-type sim \                # 所有节点注册的 instance type
  --template-preload 0.3 \             # 每个模板预置本地副本的节点比例（默认 0.3，
                                       # 对齐 template_storm 实验口径：30% 节点预置）
  --template-size-bytes 1073741824 \   # 模拟模板尺寸（默认 1GiB；template_id 打分
                                       # 是布尔的：有副本=100/无副本=0，尺寸只进
                                       # image_id 因子的尺寸×副本比例记账）
  --seed 42 --rounds 3 \               # 第 i 轮 seed = seed+i
  --mode quality \                     # quality（默认）| performance
  -o report.json                       # 缺省写 stdout（注意此时 stdout 前有 config 噪声）
```

## 运行模式（--mode）

- **quality（默认）**：虚拟时钟 + 固定 seed，回答"新策略是否改善调度质量"，
  结果稳定可复现。调度决策走真实的 `scheduler.Select`，只记录整次决策耗时
  （`sched_latency_p50/p95/p99_ms`）。
- **performance**：回答"新框架/策略/插件增加了多少运行开销"。每次调度决策改走
  sim 侧的分阶段驱动（`pkg/scheduler/sim/perf.go`，逐阶段复刻
  `scheduler.Select`：prefilter → 快照冻结 → guards → filter → score → pick，
  含 backoff/no-candidate 语义，等价性由 `TestPerfModeMatchesQualityMode` 保证），
  对每请求记录各阶段真实墙钟耗时；报告新增 `perf` 块：
  各阶段（`prefilter/guards/filter/score/pick/total`）的 P50/P95/P99/均值、
  本轮回放墙钟时长 `wall_seconds` 与决策吞吐 `throughput_rps`（请求数/墙钟秒，
  即零排队下的调度核心吞吐）。跨轮聚合取各轮分位数的均值。
  `perf.overhead` 块记录 schedsim 进程自身在回放窗口内的开销：
  `cpu_seconds`（累计进程 CPU 时间，`/proc/self/stat` utime+stime）、
  `avg_cpu_cores`（cpu_seconds/wall_seconds，平均占用核数）、
  `peak_rss_mib`（峰值常驻内存，`/proc/self/status` VmHWM，进程生命周期高水位）、
  `heap_alloc_mib`/`heap_sys_mib`（回放结束时的 Go 堆在用/已申请，
  `runtime.ReadMemStats`）。procfs 字段仅 Linux 输出（其余平台省略），
  Go 堆指标全平台可用；跨轮聚合：CPU 秒求和、峰值 RSS 取 max、Go 堆取均值。
  注意：performance 模式仍用虚拟时钟推进到达/到期事件（不 sleep），
  "真实时钟"指对每次调度决策的墙钟测量，而非按 trace 速率实时 pacing。
- compare 模式透传 `--mode`：所有变体同模式对比，报告追加
  "Scheduling overhead" 分阶段耗时表（含进程级 CPU/内存开销行）与相对
  baseline 的开销差。

```bash
schedsim --mode=performance \
  --compare legacy=example.sim.yaml,burst=burst_balance.profiles.sim.yaml \
  --trace /tmp/burst.trace.json --nodes 300 --rounds 3 -o perf-compare.md
```

## 多策略对比（--compare）

同一 trace、同一模拟集群、同一 seed 下对比多个调度配置（例如 legacy 配置
vs 基于 `scheduler.profiles` 的插件策略），一次命令产出 Markdown A/B 报告：

```bash
schedsim \
  --compare legacy=cmd/schedsim/example.sim.yaml,spread=cmd/schedsim/example.profiles.sim.yaml \
  --trace /tmp/storm.trace.json --nodes 300 --template-preload 0.3 \
  --seed 42 --rounds 3 -o compare.md --out-dir /tmp/sim-out
```

- `--compare` 取逗号分隔的 `名字=配置路径` 列表，**第一个为 baseline**；
- 每个变体跑一个独立子进程（config/scheduler/localcache 都是进程级单例，
  无法在同进程内重建），变体间只有调度配置不同，其余变量全部受控；
- 每个变体的单跑 JSON 落在 `--out-dir`（缺省为临时目录），可直接喂给
  `cube-bench compare` 与真机结果交叉对比；
- Markdown 报告含：每指标逐轮均值±95% 置信区间半宽（1.96·σ/√n，n≥2 时展示，
  与 `cube-bench compare` 同口径）、相对 baseline 的 Δ%、
  按指标方向判定的 improved/regressed 结论（|Δ%| ≥ 5% 才列入）；
  方向依赖策略目标的指标（分配率、活跃/空节点数）只报数值不下结论。

## 与 cube-bench 的衔接

trace 文件是跨工具契约（字段名双方冻结，见
`examples/cube-bench/sequence.go` 与 `pkg/scheduler/sim/trace.go`）：

```bash
# 生成 trace（cube-bench 是独立 module；--dry-run 离线生成，不碰真机）。
# 规格标注必须经 --templates id:weight:cpuMillis:memMiB 给出，否则
# cpu_millis/mem_mib 为 0，schedsim 会拒绝加载。
cd examples/cube-bench
go run . --workload template_storm --total 500 --dry-run --no-tui \
  --templates "tpl-a:3:1000:2048,tpl-b:2:2000:4096,tpl-c:1:4000:8192" \
  --dump-trace /tmp/storm.trace.json

# 同一份 trace：真机侧去掉 --dry-run 接上 --api-url 跑 cube-bench；仿真侧：
cd ../../CubeMaster
go run ./cmd/schedsim --trace /tmp/storm.trace.json \
  --config cmd/schedsim/example.sim.yaml --nodes 300 --template-preload 0.3 \
  -o /tmp/storm.sim.json
```

`cpu_millis`/`mem_mib` 为 0 的请求会被拒绝加载并提示 trace 缺少规格标注。
多轮结果取各轮均值落在 `summary`，逐轮明细在 `rounds[]`。

报告 `config` 块还记录两项运行元数据（均为可选字段，旧版二进制产出的报告
可能缺失，消费方须容忍缺省）：`version`（构建时 Go 工具写入的 vcs
revision，工作区有未提交改动时带 `-dirty` 后缀；`cube-bench compare` 的
config highlights 会展示它）与 `metric_sync_interval`（实际生效的
`scheduler.metric_update_timeout`，单位秒——即调度核心判定节点指标新鲜度
的窗口）。

## 设计

### 只动现有导出 API

仿真器不修改 scheduler/selector/localcache 的任何文件，全部通过现有导出面驱动：

- `config.Init()`（经 `CUBE_MASTER_CONFIG_PATH` 指向 `--config`）装载真实调度配置；
- `task.InitTask` + `scheduler.InitScheduler` 注册与线上完全相同的
  prefilter / filter / score / backoff 管线；
- `localcache.UpsertNode` 注入节点元数据，
  `localcache.UpdateNodeMetricInProcess` 在每次放置/到期后回写配额用量，
  `localcache.RegisterTemplateReplica` / `DeregisterTemplateReplica` 管理模板副本；
- `scheduler.Select` 做每一次选点，**不调** `localcache.Init` —— 不碰
  DB/Redis、不起后台同步协程（包级缓存单例在包加载时已就绪）。

### 注入的节点字段

`InsID`（`schedsim-r<round>-n<i>`，轮次前缀避免残留冲突）、`IP`、
`Healthy/ReportedReady=true`、`InstanceType`、`ClusterLabel/OssClusterLabel`
（`schedsim-<instanceType>`）、`QuotaCpu`（毫核）/`QuotaMem`（MiB）、
`CpuTotal`/`MemMBTotal`（物理总量=配额）、`MetaDataUpdateAt/MetricUpdate/
MetricLocalUpdateAt=now`。用量侧（`QuotaCpuUsage/QuotaMemUsage/MvmNum`）由
仿真账本维护并实时镜像进 localcache。

### metric 新鲜度（坑 1）

prefilter 用真实时钟检查 `metric_update_timeout`，而仿真跑虚拟时钟。双保险：
example.sim.yaml 把 `metric_update_timeout` 调到 86400s，同时引擎每次放置/
到期都会用 `time.Now()` 回写节点 metric（顺带刷新 `MetricUpdate`/
`MetricLocalUpdateAt`）。一轮通常在秒级真实时间内跑完，永不超时。

### 虚拟时钟与时间平均

创建事件（`arrival_ms`）与到期事件（`arrival_ms+lifetime_ms`）进同一个最小堆，
按虚拟时间弹出；同一时刻到期先于创建（先释放资源再准入）。**调度本身不 sleep**，
10 万请求也能秒级跑完。每次事件处理完后对集群状态采样，样本按它在虚拟时间上
持续的时长加权（`TimeWeightedAvg`），因此"1ms 的毛刺"与"1 小时的平台期"权重
天差地别——这是装箱率/CV/Jain/碎片/活跃节点数的正确打开方式。

### 随机性与复现（坑 2）

- trace 本身是确定输入；模板预置副本由 `math/rand.New(NewSource(seed))`
  抽取，逐轮 seed = `--seed + i`，**完全可复现**。
- 终选随机性：`selctx.New` 内部的 `randomSelect` 用 `golang.org/x/exp/rand`
  以 `time.Now()` 播种，外部无法固定；`LeastRandomSelect` 在
  `priority_select_num > 1` 时从前 N 名中均匀随机，**逐次运行不可复现**。
  example.sim.yaml 设 `priority_select_num: 1`（确定性取最高分）规避。
- 残余非确定性：未调 `localcache.Init` 时节点枚举走 go-cache map 遍历，
  分数完全并列时的 tie-break 次序逐次运行可能不同。该路径不可在不修改
  localcache 的前提下固定，故结论性指标请依赖 `--rounds` 多轮聚合
  （各轮独立 seed，报告顶层 summary 即逐轮均值）。
- `BackoffSelect` 的 `math/rand` 全局源在仿真里不可达（无
  BackoffNodeSelector 时 backoff 恒为空集），无需 seed。

### 指标定义

| key | 定义 |
| --- | --- |
| `success_rate` | Select 成功数 / 请求总数 |
| `sched_latency_p50/p95/p99_ms` | 每次 `Select` 的真实墙钟耗时（含失败调用），最近秩分位 |
| `cpu_alloc_rate` / `mem_alloc_rate` | Σ 用量 / Σ 原始配额（集群级，时间平均；超卖下可 >1 的部分由 effective 配额承载，此处按原始配额） |
| `load_cv_cpu` / `load_cv_mem` | 节点用量率（用量/配额）总体标准差 / 均值；均值为 0 定义为 0 |
| `jain_cpu` / `jain_mem` | Jain 公平指数 (Σx)²/(n·Σx²)；全 0 定义为 1（完全均衡） |
| `fragmentation_ratio` | 对 trace 中**最大请求 shape**（max cpu_millis）：放不下该 shape 的节点的空闲 CPU 占总空闲 CPU 的比例。"空闲"与 cpu filter 同口径（配额×超卖比−已分配），"放不下"与 filter 的 `free > req` 判定互补（`free <= maxShape`） |
| `herding_top1_share` | 被选中次数最多的节点占总成功放置的比例（羊群度） |
| `template_hit_rate` | 成功放置中选中节点持有该模板本地副本的比例（分母为带模板的成功请求）。`AllowNonLocalTemplate=true` 时模拟缓存升温：远程落盘的模板随后在服务节点注册本地副本，同模板后续创建可命中，该指标因此能区分 locality 策略收益；`AllowNonLocalTemplate=false` 且配合 template_locality filter 时恒为 1（该形态下仅用于检测配置漂移） |
| `active_nodes_avg` / `empty_nodes_avg` | 有/无运行中沙箱的节点数，时间平均 |

指标计算均为纯函数（`pkg/scheduler/sim/metrics.go`），单测手算对拍。

## 验证

```bash
cd CubeMaster
go build ./cmd/schedsim/... ./pkg/scheduler/sim/...
go test ./pkg/scheduler/sim/...
GOOS=linux GOARCH=amd64 go build ./cmd/schedsim
```

e2e 测试（`engine_test.go`）覆盖：单节点全集中（cv=0、jain=1、
cpu_alloc_rate 对手算积分）、4 节点 8 请求 least-loaded 均分
（2-2-2-2、top1=0.25）、preload=0 时模板请求全部被 locality filter 拒绝
（success_rate=0）。
