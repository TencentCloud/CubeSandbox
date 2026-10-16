# External HTTP Score — Operator VERIFY

按启动 → 配置 → 验证 → 故障/回滚执行。工作目录：`CubeMaster/`。

## 1) 启动 scorer（一条命令）

```bash
go run ./examples/external-http-score
```

探活：`curl -s http://127.0.0.1:18080/healthz` → `ok`

## 2) 配置片段（复制到 CubeMaster conf `scheduler.score`）

```yaml
enable_scorers:
  - external_http_score
plugin_conf:
  external_http_score:
    endpoint: http://127.0.0.1:18080/score
    weight: 1
    timeout: 200ms
    # mode: mock_metrics   # optional: business-signal demo via /mock-metrics
```

改 `enable_scorers` / Profile 后需 **重启 CubeMaster**（selector 不热重建）。`disable: true` 可在 conf 热更新路径关闭 scorer（以现场二进制为准）。

## 3) 验证（一条命令）

```bash
curl -s http://127.0.0.1:18080/score \
  -H "Content-Type: application/json" \
  -d '{"mode":"mock_metrics","nodes":[{"node_id":"node-a"},{"node_id":"node-b"}]}'
```

期望：`node-b` 分数高于 `node-a`（默认 mock 快照）。  
业务信号翻转：

```bash
curl -s -X POST http://127.0.0.1:18080/mock-metrics \
  -H "Content-Type: application/json" \
  -d '{"nodes":{"node-a":{"cpu_utilization":10,"memory_utilization":10,"sandbox_count":1,"estimated_create_latency_ms":20},"node-b":{"cpu_utilization":90,"memory_utilization":90,"sandbox_count":30,"estimated_create_latency_ms":300}}}'
```

再跑同一 `/score`：期望 `node-a` > `node-b`。

本地单测：`go test ./examples/external-http-score -count=1`

CubeMaster 指标（启用后）：`cube_scheduler_external_http_score_outcomes_total{reason=...}`

## 4) 故障注入与回滚（一步回滚）

| 场景 | 注入 | 期望 |
|---|---|---|
| healthy | `/fault` delay=0 | reason≈success |
| delayed | `POST /fault {"delay_ms":80}` | 延迟增加，仍可能 success |
| timeout | `delay_ms` > conf timeout | reason=timeout，**fail-open 创建继续** |
| non2xx | `{"http_status":503}` | reason=http_status，fail-open |
| bad scores | `{"bad_scores":true}` | reason=missing_candidate 等，fail-open |
| 回滚 | `POST /fault {"delay_ms":0,"http_status":0,"bad_scores":false}` + `POST /mock-metrics/reset`；配置侧去掉 `external_http_score` 或设 `disable:true` 后按现场重启策略 | 恢复原路径 |

**持续故障与默认熔断：** 上表 `timeout` / `http_status` / `missing_candidate` 等单 `reason` 描述的是熔断跳闸前的前几次尝试（默认 `failure_threshold: 5`）。若故障持续注入，熔断打开后 outcome 变为 `reason=circuit_open`（不再发 HTTP）。要再观察原始 reason，请清掉故障，或设 `circuit_breaker.disable: true`。

**Filter-before-Score**：scorer 只能给 Filter 后候选打分；响应里的陌生 `node_id` 不会复活已过滤节点。

## 场景/回滚表（摘要）

| Arm | 动作 | 回滚 |
|---|---|---|
| control | 不启用 scorer | n/a |
| healthy | 启用 + fault 清零 | disable / 恢复 conf |
| delayed | fault delay_ms | `/fault` 清零 |
| timeout | delay > timeout | `/fault` 清零 |
| signal | mock-metrics patch | `/mock-metrics/reset` |
