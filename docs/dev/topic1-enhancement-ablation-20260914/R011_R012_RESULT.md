# R011 / R012 结果摘要（2026-09-14 live）

结论包入口：[`README.md`](./README.md)。原始机房 ledger **未**纳入本 PR。

## 变体

| | S0 | S1 |
|--|----|----|
| 角色 | 产品化线 staged 二进制（无熔断） | 熔断 + `failure_policy: fail_open` |
| 熔断参数 | 无 | `failure_threshold=3`, `open_duration=60s` |
| 集群恢复 | — | **RESTORE PASS**（stock 二进制 + stock conf） |

## timeout 臂（主主张 A1）

| 指标 | S0 | S1 | Δ |
|------|----|----|---|
| success | 20/20 | 20/20 | 持平（fail-open） |
| api_p50_ms | 556.5 | 315.0 | **-43%** |
| api_p95_ms | 708.05 | 535.15 | **-24%** |
| api_p99_ms | 708.81 | 674.23 | **-5%** |
| sidecar HTTP 次数 | 22 | **3** | 熔断后短路 |
| timeouts_total | 22（reason:timeout） | **3** | |
| failures_total | — | 22（含 open reject） | |
| latency_sum_s | 4.413 | **0.603** | **-86%** |
| circuit_state | n/a | 结束为 Open(2) | |

结论：熔断打开后不再对每次 create 打满默认 HTTP 超时；create 仍全部成功。A1（熔断短路持续超时尾延迟，fail-open 保持可创建）在本窗成立。无独立负缓存；短路机制是 circuit open。

## healthy 臂（R012 回归）

| 指标 | S0 | S1 |
|------|----|----|
| success | 20/20 | 20/20 |
| api_p95_ms | 455.7 | 439.45 |
| requests | 22 | 22 |
| failures | 0 | 0 |
| circuit_state | n/a | 0（Closed） |

健康路径熔断保持闭合，未伤成功率。

## delayed 臂（旁证，非主主张）

S1 delayed 的 api_p95/p99 **高于** S0（744/1038 vs 591/627），属噪声或与人为延迟叠加的实验方差；metrics 显示仍为成功路径、circuit Closed。**不**据此否定 A1。

## 单测（同窗）

- S0 score/scheduler ExternalHTTP 相关：PASS  
- S1 Circuit / FailClosed / FailOpen / Reliability：PASS  

## 本窗未做

- 负缓存实现或验证  
- S2 并行 / 异步缓存实验  
- S3 异构主表重跑 / 新迟滞规则  
