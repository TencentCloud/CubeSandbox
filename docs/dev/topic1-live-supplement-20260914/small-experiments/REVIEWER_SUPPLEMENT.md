# Reviewer Supplement — Live Mechanism Evidence (不改正式判定)

**性质**：正式 capacity-frontier 之外的 **补充机制证据**。  
**正式结论不变**：`binpack_result = VALID_NO_IMPROVEMENT`；`topic1_official_acceptance = TOPIC1_READY`。  
**本目录**：本 PR 入库的结论包（STATUS / summaries）。完整原始 ledger **未**纳入本 PR。

---

## 五项实验（名称即内容）

| 实验（突出内容） | 机器键 | 状态 | 一句话 |
|---|---|---|---|
| **真实多 VM：创建→Ready→首命令端到端延迟** | `vm_e2e_latency` | PASS | default vs `binpack_utilization`，warm-up 后各 10 次顺序 + 5 并发突发；客户端时间线，非 simulator |
| **突发短创建：节点分散与启动延迟** | `burst_scenario` | PASS | 8 并发 `burst_unit`；比较热点节点最大并发与 usable 延迟 |
| **镜像局部性：部分节点有缓存 / 部分无缓存的命中对照** | `locality_scenario` | NO_IMPROVEMENT | 已证明 cache contrast；locality profile 未抬高命中率 |
| **CPU/内存错配装箱：多维碎片、空节点、探针接纳、e2e 延迟** | `binpack_scenario` | NO_IMPROVEMENT | 同序 cpu_heavy/mem_heavy 填满后探针；binpack 空节点↑，接纳率打平；**不覆盖**正式 VALID_NO_IMPROVEMENT |
| **外部 HTTP scorer：正常 / 超时 / 非2xx / 非法分 的退化** | `external_scorer_degradation` | PASS | ok=3/3；故障模式 fail-closed；Filter 后候选未复活 |

恢复：`RESTORE.json` = **PASS**（stock `edf40cb9…`，conf `a0ac521b…`，sandbox=0，18080 stub 已停）。

---

## 可粘贴到 PR / mentor 回复的短文

```text
## Live supplement (2026-09-13) — mechanism evidence only

Formal capacity-frontier (CF01–CF06) remains:
  binpack_result = VALID_NO_IMPROVEMENT
  topic1_official_acceptance = TOPIC1_READY

Additional live multi-VM observations (in-tree conclusion pack):
  docs/dev/topic1-live-supplement-20260914/small-experiments/

  1. Real multi-VM create→Ready→first-command e2e latency — PASS
  2. Burst create: node dispersion & startup latency — PASS
  3. Image locality: cached vs uncached node hit contrast — NO_IMPROVEMENT
  4. CPU/mem mismatch packing: fragmentation / empty nodes / probe admission — NO_IMPROVEMENT
     (does NOT override formal VALID_NO_IMPROVEMENT)
  5. External HTTP scorer degrade: ok / timeout / non-2xx / bad score — PASS
     (fail-closed on errors; no Filter resurrection)

Entry: REVIEWER_SUPPLEMENT.md  Status: STATUS.json  Verify: VERIFY.md
Runnable operator path: CubeMaster/examples/external-http-score/VERIFY.md
```

---

## 复核

见 [`VERIFY.md`](./VERIFY.md)。
