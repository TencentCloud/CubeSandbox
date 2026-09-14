# Topic1 小实验补充收口

**给 reviewer 的一页入口**：[`REVIEWER_SUPPLEMENT.md`](./REVIEWER_SUPPLEMENT.md)（实验名突出内容；正式判定不变）

本目录是 **结论包**（STATUS / summaries / VERIFY）。完整原始 ledger 未纳入本 PR。

## 五项实验（名称 = 内容）

| 实验 | 状态 |
|---|---|
| **真实多 VM：创建→Ready→首命令端到端延迟** (`vm_e2e_latency`) | **PASS** |
| **突发短创建：节点分散与启动延迟** (`burst_scenario`) | **PASS** |
| **镜像局部性：有缓存/无缓存命中对照** (`locality_scenario`) | **NO_IMPROVEMENT** |
| **CPU/内存错配装箱：碎片·空节点·探针接纳·延迟** (`binpack_scenario`) | **NO_IMPROVEMENT**（正式仍为 VALID_NO_IMPROVEMENT） |
| **外部 HTTP scorer：正常/超时/非2xx/非法分退化** (`external_scorer_degradation`) | **PASS** |

恢复：`RESTORE.json` = **PASS**

## 边界

- 补充机制证据；**不**覆盖正式 CF01–CF06 / `VALID_NO_IMPROVEMENT`
- 可复现的操作路径见 `CubeMaster/examples/external-http-score/VERIFY.md`，**不是**本目录的 ledger 脚本

## 复核

见 [`VERIFY.md`](./VERIFY.md)。结论文件：`summaries/*.json`、`STATUS.json`
