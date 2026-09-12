# 调度出厂策略：4 节点真实集群负载压测

> 分支：`feat/scheduler-eval-plugin`（go:embed 出厂调度策略注入）
> 日期：2026-09-08 | 环境：4 × 8C/15G x86_64 真实节点，真实 KVM 数据面，全链路真实组件

本报告记录对"调度出厂策略嵌入"改动的真实集群负载验证：CubeMaster 的 `conf.yaml` **完全不写** `profiles` / `filter` / `score` 任何调度配置，仅靠 `go:embed` 内置的出厂 YAML（`CubeMaster/pkg/base/config/scheduler_factory.yaml`），三条内置策略（`burst_balance` / `template_reuse` / `mixed_binpack`）在真实 agent 式负载下全部生效。与上一轮的[控制面 A/B 基准](/zh/dev/scheduler-eval-benchmark)（真实控制面 + 模拟数据面）不同，本轮是**真实 4 节点集群 + 真实 KVM 沙箱**的端到端压测。

## 拓扑与被测对象

```
agent_load.py ──HTTP──▶ CubeMaster(:8089, any15, 本分支构建 v0.7.0-factoryprofiles)
(100 并发会话)            │  conf.yaml 无 filter/score/profiles → 出厂注入生效
                          ▼ gRPC
                kubelet × 4（any11/13/15/16，各 8C/15G，真实 KVM 微虚机）
```

- 测试模板 `tpl-71ba92400dda437e9ea8ba19`：500m CPU / 512Mi 内存 / 512M writable，已 `tpl redo` 预分发到 4/4 节点（`template_locality` 强制守卫要求本地副本）。
- 沙箱创建经 `POST /cube/sandbox` + `workload` label 路由：`burst_balance`→打散、`template_reuse`→模板本地性、`default` 及其他→`mixed_binpack`。

## 负载模型（模拟真实 agent 环境）

| 阶段 | 负载 | 目的 |
|---|---|---|
| 1. churn | 100 个并发 agent 会话 × 360s；每会话 = 创建沙箱 → 工作 20-60s → 销毁 → 思考 0.5-3s；label 混合 50% default / 30% burst / 20% reuse | 稳态混部 |
| 2. burst wave | 80 个 `burst_balance` 沙箱同时创建 | 突发开工潮 |
| 3. 饱和探测 | default 标签连续创建，直到连续 10 次失败（上限 160） | 摸集群容量天花板 |
| 4. drain | 销毁全部存活沙箱 | 验证回收 |
| 5. recovery | 混合 label 再创建 20 个 | 验证饱和后调度器恢复 |

## 结果总览

**1179 次调度操作，0 失败**；峰值并发沙箱 **240 个**（drain 阶段统计）仍未触顶（饱和探测打满 160 上限，连续失败次数始终为 0）。

客户端时延（`POST /cube/sandbox` 响应耗时，即调度决策 + 受理；微虚机随后异步就绪）：

| 阶段 | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| churn | 919 | 49ms | 55ms | 61ms | 133ms |
| burst wave（80 并发） | 80 | 168ms | 419ms | 840ms | 840ms |
| 饱和探测（240 在线时） | 160 | 168ms | 230ms | 243ms | 694ms |
| recovery | 20 | 68ms | 84ms | 86ms | 86ms |

## 策略路由与放置分布

调度日志 `profile=` 命中共 1211 行：`mixed_binpack` 645 / `burst_balance` 368 / `template_reuse` 198（含内部重试，比例 ≈ 53%/30%/16%，与注入的 50/30/20 流量混合一致），三条出厂策略全部真实参与调度。

成功操作的 label × 节点分布（创建与销毁成对计入，反映放置形状；IP 尾号：any11=.6，any13=.28，any15=.228，any16=.31）：

| 策略 | any11 | any13 | any15 | any16 | 行为判定 |
|---|---|---|---|---|---|
| burst_balance | 105 | 60 | 28 | 73 | 4 节点打散，churn 期各节点 mvm 稳定在 17–30 区间漂移 |
| mixed_binpack | 93 | 156 | 186 | 189 | 明显集中；饱和阶段 160 个创建中 100 个堆到 any16、any15 为 0——典型 binpack 填充签名 |
| template_reuse | 65 | 39 | 26 | 59 | 4 节点均有本地副本，选其中最闲者 |

两个值得记录的设计行为：

- **binpack 的倾斜有依据**：any11 磁盘占用 21%（其余节点 8–9%），`resource_fit_score` 打分被压低，default 流量显著更少落到 any11——score 插件在起作用，不是路由错误。
- **template_locality 是硬守卫**：模板必须 `tpl redo` 分发到节点后该节点才参与 `template_reuse` 调度；未分发的节点会被整体滤掉（本轮 4/4 已分发，故 4 节点均摊）。

## 备注与已知局限

- 压测驱动在 burst wave / recovery 两个阶段把 sandbox id 误记入 host 列（驱动小 bug），这两个阶段的逐节点分布未直接统计；上表分布数据来自 churn 与饱和阶段。不影响成功率与时延结论。
- 240 并发未触顶：500m/512Mi 小规格下单节点实际承载 60+ 沙箱（约 4× CPU 超售），集群真实上限高于本次探测上限 160。
- 集群按项目需要**保持运行未复原**；驱动与原始数据在 any15：`/root/agent_load.py`、`/root/load/{ops.csv,nodes.csv,driver.log}`。压测驱动为一次性实验代码，未合入仓库。
