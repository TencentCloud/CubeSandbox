# BUSINESS_VALUE

## Operator journey
最短路径已落到 CubeMaster/examples/external-http-score/VERIFY.md：一条启动、一段配置、一条验证、故障/回滚表。

## Hot-path cost (client API latency)
- control api_p95 ≈ 590.8 ms
- healthy api_p95 ≈ 449.4 ms
- delta (healthy-control) = -141.45 ms（工程目标 ≤50ms）
- delayed/timeout arms 全成功；timeout arm 在 fail-open 下仍可创建

## Business signal
mock_metrics 偏好节点命中 6/6，证明外部分数可改变合法候选排序。

## Filter-before-Score
单元合同 + live bad_scores 臂记录；scorer 不能复活 Filter 外节点。

## Not claimed
不覆盖正式 binpack VALID_NO_IMPROVEMENT；未跑可选错配 probe。

## E5 CPU/mem mismatch probe (NO_IMPROVEMENT)
- default empty_nodes=1 probe_rate=1.0 frag=0.1578947368421053
- binpack empty_nodes=3 probe_rate=1.0 frag=0.1578947368421053
- reasons=['binpack_more_empty_nodes']
- formal_override=false; formal remains VALID_NO_IMPROVEMENT
