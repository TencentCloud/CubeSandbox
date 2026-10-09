# BUSINESS_VALUE

## Operator journey
最短路径已落到 CubeMaster/examples/external-http-score/VERIFY.md：一条启动、一段配置、一条验证、故障/回滚表。

## Hot-path cost (client API latency)
- control api_p95 ≈ 590.8 ms
- healthy api_p95 ≈ 449.4 ms
- delta (healthy-control) = -141.45 ms — **not** a ≤50 ms scorer-cost proof (arm variance / uncontrolled A/B)
- delayed api_p95 ≈ 623.4 ms；timeout api_p95 ≈ 683.8 ms（相对 healthy 可观测注入成本）
- delayed/timeout arms 全成功；timeout arm 在该次 productization binary 上 create 仍成功（fail-open 路径）；勿与 small-experiments 的 fail-closed 混称

## Business signal
mock_metrics 偏好节点命中 6/6，证明外部分数可改变合法候选排序。

## Filter-before-Score
单元合同 + live bad_scores 臂记录；scorer 不能复活 Filter 外节点。

## Not claimed
不覆盖正式 binpack VALID_NO_IMPROVEMENT；未声明 healthy-vs-control ≤50 ms 统计界；未声明全 lab 统一 fail-open。

## E5 CPU/mem mismatch probe (NO_IMPROVEMENT)
- default empty_nodes=1 probe_rate=1.0 frag=0.1578947368421053
- binpack empty_nodes=3 probe_rate=1.0 frag=0.1578947368421053
- reasons=['binpack_more_empty_nodes']
- formal_override=false; formal remains VALID_NO_IMPROVEMENT
