# External HTTP Score 插件

`external_http_score` 让 CubeMaster 在调度 Score 阶段调用外部 HTTP 服务。该服务为每个候选节点返回 0–100 分，CubeMaster 再把这些分数送入现有加权评分流水线。

除非把该插件列入 `scheduler.score.enable_scorers`，否则它不会启用。启用 `enable_scorers: external_http_score` **必须**提供匹配的 `score.plugin_conf.external_http_score` 块；否则 CubeMaster 在启动构造 scorer 时会 panic。它不作为 Filter 运行，不会剔除候选，也不直接决定最终节点。

面向运维的配置说明见 [CubeMaster 调度配置](../guide/cubemaster-scheduler-config.md#external-http-score-插件)（中文）/ [English](../../guide/cubemaster-scheduler-config.md#external-http-score-plugin)。

## 配置

```yaml
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      mvm_num: 1
    plugin_conf:
      external_http_score:
        weight: 1.0
        endpoint: "http://127.0.0.1:18080/score"
        timeout: 200ms
        mode: ""
        disable: false
        failure_policy: fail_open   # 省略时默认
        circuit_breaker:
          disable: false
          failure_threshold: 5
          open_duration: 5s
          half_open_max_probes: 1
```

字段：

- `weight`：在 `runScoreFilter` 中的相对权重。省略默认为 `1.0`；显式 `0` 为静默空操作。
- `endpoint`：绝对 `http://` / `https://` sidecar URL。正 weight 下为空会可观测地 fail-open。
- `timeout`：同步 create 路径上的单次请求超时（默认 `200ms`，上限 `2s`）。
- `mode`：透传给 sidecar 的不透明字符串。
- `disable`：为 true 时 scorer 为空操作。
- `failure_policy`：sidecar 失败处理。
  - **`fail_open`（默认**，含空/未知值）：`Select` 返回普通错误；`runScoreFilter` 跳过该 scorer 并继续调度（历史 create 路径行为）。
  - **`fail_closed`**：`Select` 返回类型化 `FailClosedError`；`runScoreFilter` 中止 Score，create 失败关闭。API 客户端看到 `ErrorCode_SelectNodesFailed` 与脱敏类别消息（不是 `ErrorCode_Unknown`）。
- `circuit_breaker`：连续 sidecar 失败后打开熔断，后续 Score 立即失败而不再等待完整 HTTP 超时。经过 `open_duration` 后允许半开探测（`half_open_max_probes`，默认 1）；成功关闭熔断，失败重新打开。设 `disable: true` 可关闭熔断。热更新删除该块或将字段置 0 会恢复下列默认值（参数不会只升不降）。

省略该块或字段为 0 时的默认熔断值（除非 `disable: true`，否则熔断仍启用）：

- `failure_threshold`: 5
- `open_duration`: 5s
- `half_open_max_probes`: 1

## 生产可靠性

sidecar 故障不应把每次调度尝试都钉在 HTTP 超时上。连续 `failure_threshold` 次请求/响应失败后熔断打开，`Select` 立即返回（`fail_closed` 类型错误，或普通 fail-open 错误）。开路拒绝不发 HTTP，映射为 outcome reason `circuit_open`。

共享 transport 用 `MaxConnsPerHost = 8` 限制在途连接。超额并发 Select 会在拨号队列中等待并消耗单次 `timeout`；创建突发下即使 sidecar 健康也可能呈现超时并触发熔断。除非并发已按约 `8 / p50 延迟` 约束，否则请保持默认 `fail_open`。

Prometheus 指标（CubeMaster 进程）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `cube_scheduler_external_http_score_outcomes_total{reason=...}` | counter | 按固定 reason 的结果（`success`、`timeout`、`connection`、`http_status`、`invalid_json`、`missing_candidate`、`circuit_open`、`other`） |
| `cube_scheduler_external_http_score_request_duration_seconds{reason=...}` | histogram | 按 outcome reason 的 HTTP 往返延迟（开路拒绝不采样） |
| `cube_scheduler_external_http_score_circuit_state{target=...}` | gauge | `0=closed`，`1=half-open`，`2=open`；`target` 仅为 host:port |

实现：`CubeMaster/pkg/selector/score/externalhttpscore.go`、`externalhttpscore_breaker.go`、`externalhttpscore_metrics.go`、`failclosed.go`。

## 请求 / 响应

见调度配置指南中的线协议约定。每个请求的候选都必须收到 `[0, 100]` 内的有限分数；额外 key 忽略；候选集不完整会使整次尝试失败。
