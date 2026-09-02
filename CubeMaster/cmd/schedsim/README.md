# schedsim — 调度仿真器

schedsim 用**模拟节点状态**在单个进程内驱动 CubeMaster 真实的调度核心
`scheduler.Select`，回放 cube-bench 生成的请求 trace，用于在无真实集群的
条件下大规模评估调度质量：装箱率、碎片率、负载均衡、羊群度、模板命中率、
调度延迟。

```
cmd/schedsim/main.go        CLI 入口（flag 编排、逐轮驱动、报告输出）
cmd/schedsim/example.sim.yaml  示例调度配置（可自由编辑；仅 parse-only 冒烟测试守护其可解析）
pkg/scheduler/sim/          可测逻辑：trace 解析、虚拟时钟事件循环、指标纯函数
pkg/scheduler/sim/testdata/sim.test.yaml  引擎测试自用的示例冻结副本（有意改示例时重新同步它）
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
  --template-preload 0.3 \             # 每个模板预置本地副本的节点比例
  --seed 42 --rounds 3 \               # 第 i 轮 seed = seed+i
  -o report.json                       # 缺省写 stdout（bootstrap 的 config 输出已被屏蔽，stdout 直接是干净 JSON）
```

## 与 cube-bench 的衔接

trace 文件是跨工具契约（字段名双方冻结，见
`examples/cube-bench/sequence.go` 与 `pkg/scheduler/sim/trace.go`）。
注意：生产者侧（cube-bench `--dump-trace`）随配套 PR
[TencentCloud/CubeSandbox#1623](https://github.com/TencentCloud/CubeSandbox/pull/1623)
落地；在它合并之前，可先用本目录的 `example.trace.json`（6 个带规格标注的
请求）做独立冒烟：

```bash
cd CubeMaster
go run ./cmd/schedsim --trace cmd/schedsim/example.trace.json \
  --config cmd/schedsim/example.sim.yaml --nodes 4 --template-preload 1.0
```

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
加载时**不会**交叉校验每个请求的 `template_id` 是否出现在 `templates` 段：
引用了未列出模板的请求永远不会被预置副本，在严格 locality（默认
`--allow-non-local-template=false` 且 locality filter 开启）下会全部失败且
无加载期报错——排查 `success_rate=0` 时先检查这一点。
多轮结果取各轮均值落在 `summary`，逐轮明细在 `rounds[]`——注意分位指标
（`sched_latency_p50/p95/p99_ms`）也是**逐轮分位的均值**，不是全样本合并后的
真实分位；要看分布形状请读 `rounds[]`。

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

### 真实用量门禁不生效（口径与局限）

仿真只镜像**配额口径**的用量（`QuotaCpuUsage/QuotaMemUsage/MvmNum`），从不更新
真实用量字段（`MemUsage`/`CpuUtil`）与实时创建计数（`RealTimeCreateNum`/
`LocalCreateNum`），因此三条真实集群门禁在仿真里恒不触发：

- mem filter 的物理内存检查（`MemMBTotal - MemUsage > req + reserved`）恒过——
  配合示例配置（物理=配额、`mem_ratio: 2.0`），单节点可被分配到约 2× 物理内存；
- cpu filter 的 `CpuUtil >= NodeMaxCpuUtil` 守卫恒不触发；
- `realtime_create_num` filter 恒不拒绝。

另有一条被**刻意抬高**而非失效的门禁：prefilter 的节点并发沙箱数上限
（`MvmNum >= RealMaxMvmLimit`，生产默认 3000/节点）。密集 trace（如 10 万请求
铺在 300 节点上且 lifetime 较长）会先撞上这条上限、悄无声息地压低
`success_rate`，因此示例配置把它调到 `node_max_mvm_num: 100000`，让配额门禁
——被测对象——先触发；评估更密集场景时记得真实集群在这条上限处就开始拒绝。

结论：`mem_alloc_rate`/`load_cv_mem` 等绝对值可能超出真实集群可达范围，
仿真结果用于**同配置族的相对 A/B 对比**，不要直接对标生产的绝对容量上限。

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

时间平均类指标（`cpu/mem_alloc_rate`、`load_cv_*`、`jain_*`、`fragmentation_ratio*`、
`active/empty_nodes_avg`）的积分窗口固定为 [0, max(arrival_ms) + max(lifetime_ms))，
由 trace 唯一决定、与调度成功率无关：被拒绝的请求不产生到期事件，若窗口止于最后
一个事件，拒绝率高的配置会在更短的分母上积分，A/B 对比就被污染了。

| key | 定义 |
| --- | --- |
| `success_rate` | Select 成功数 / 请求总数 |
| `sched_latency_p50/p95/p99_ms` | 每次 `Select` 的真实墙钟耗时（含失败调用），最近秩分位 |
| `cpu_alloc_rate` / `mem_alloc_rate` | Σ 用量 / Σ 原始配额（集群级，时间平均；超卖下可 >1 的部分由 effective 配额承载，此处按原始配额） |
| `load_cv_cpu` / `load_cv_mem` | 节点用量率（用量/配额）总体标准差 / 均值；均值为 0 定义为 0 |
| `jain_cpu` / `jain_mem` | Jain 公平指数 (Σx)²/(n·Σx²)；全 0 定义为 1（完全均衡） |
| `fragmentation_ratio` | 对 trace 中最大**可行**请求 shape（max cpu_millis；连空节点都放不下的请求被排除——节点同构，`cpu_millis >= 配额×cpu_ratio` 或 `mem_mib >= 配额×mem_ratio` 的请求到哪儿都会被拒，计入会把所有节点判成 unfit、让指标恒 ≈1，度量的是离群请求而非调度碎片；整份 trace 均不可行时回退为全 trace 最大值）：放不下该 shape 的节点的空闲 CPU 占总空闲 CPU 的比例。"空闲"与 cpu filter 同口径（配额×超卖比−已分配），"放不下"与 filter 的 `free > req` 判定互补（`free <= maxShape`） |
| `fragmentation_ratio_mem` | 上者的内存侧版本（max mem_mib，同样的可行性排除，与 mem filter 同口径）。两个指标分开跟踪：取决于超卖比与请求 shape，任一资源都可能先成为瓶颈（如 mem_ratio < cpu_ratio 且 shape 偏小时，内存先耗尽、CPU 被搁置），只看 CPU 侧会漏报 |
| `herding_top1_share` | 被选中次数最多的节点占总成功放置的比例（羊群度） |
| `template_hit_rate` | 成功放置中选中节点持有该模板本地副本的比例（分母为带模板的成功请求）。放置成功后仿真会把该模板注册到选中节点（预热拉取），因此 locality filter 关闭时该指标跟踪动态局部性（首次未命中、后续命中）；filter 开启且 `AllowNonLocalTemplate=false` 时未预热节点本就不收该模板请求，指标恒为 1，主要用于检测配置漂移 |
| `active_nodes_avg` / `empty_nodes_avg` | 有/无运行中沙箱的节点数，时间平均 |

指标计算均为纯函数（`pkg/scheduler/sim/metrics.go`），单测手算对拍。

已知限制：每轮结束时节点被标记为不健康但**不会从进程级 localcache 中移除**
（刻意不调用 `localcache.Init`，没有同步/淘汰回路）；节点 ID 带 RoundID 前缀，
轮间不会互相污染，但长时间在同一进程内反复跑仿真会累积陈旧节点条目——CLI
的小轮数场景无影响。唯一不中性的是 `sched_latency_*`：节点枚举（fallback 全扫
路径）的开销随累积条目增长，**多轮 run 内靠后轮次的延迟分位会被轻微抬高**，
跨配置对比请以相同 `--rounds`/`--nodes` 形状为前提，并按相同轮次下标对齐
（config A 的 round i 对 config B 的 round i），不要拿 A 的首轮比 B 的末轮。

另一限制：`scheduler.ignore_redis_allocation: true` 会让 filter 的
`EffectiveAllocated` 恒为 0，调度器完全看不到仿真的进程内用量回写，配额门禁永不
触发，所有利用率/均衡指标失真——仿真配置必须保持 `false`（`Bootstrap` 检测到
`true` 会向 stderr 打印警告）。

再一限制：`config.Init` 安装的 hotswap watcher 在仿真运行期间保持活跃——
**不要在运行中编辑 `--config` 指向的配置文件**，否则正在进行的轮次会切到新
配置，同一份报告会混入两套配置的结果。

模型层面的已知简化：

- **模板预热是瞬时的**：放置成功后 `noteTemplatePlacement` 同步注册本地副本，
  下一个请求（虚拟时钟上可能 0ms 后）立即命中；真实拉取耗时且可能失败，因此
  `template_hit_rate` 是 locality 收益的**乐观上界**。
- **低 preload 会扭曲头部指标**：示例配置开着 locality filter，配合默认
  `--allow-non-local-template=false`，`--template-preload 0.3` 意味着约 70% 的
  节点永远放不下某模板——此时 `success_rate`/碎片/均衡部分度量的是副本覆盖度而
  非调度质量。要测量动态 locality（未命中→预热→命中）并解开这个扭曲，用
  `--allow-non-local-template` 或关闭 locality filter（参见指标定义表
  `template_hit_rate` 行）。
- 示例配置的 `instance_type_conf.sim → schedsim-sim` 映射当前是惰性的（未调
  `localcache.Init` 时节点枚举走 fallback 全扫，与索引无关）；若未来重构让枚举在
  Init 之前走索引路径，仿真会静默失效——届时这个映射会变成真实依赖。

## 验证

```bash
cd CubeMaster
go build ./cmd/schedsim/... ./pkg/scheduler/sim/...
go test ./pkg/scheduler/sim/...
GOOS=linux GOARCH=amd64 go build ./cmd/schedsim
```

e2e 测试（`engine_test.go`）从 `pkg/scheduler/sim/testdata/sim.test.yaml`
（示例配置的冻结副本）bootstrap，覆盖：单节点全集中（cv=0、jain=1、
cpu_alloc_rate 对手算积分）、4 节点 8 请求 least-loaded 均分
（2-2-2-2、top1=0.25）、preload=0 时模板请求全部被 locality filter 拒绝
（success_rate=0）。shipped 示例的可加载性由 parse-only 冒烟测试
（`TestShippedExampleConfigParses`）单独守护——编辑 `example.sim.yaml` 不会
破坏引擎测试；有意改动调度字段时，把变更同步进 testdata 副本即可。
