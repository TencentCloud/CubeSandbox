# External HTTP Score 运维路径

仓库内最短步骤见 `CubeMaster/examples/external-http-score/VERIFY.md`。

本页是 docs 侧入口，不改变调度语义。字段说明见 [CubeMaster 调度配置 — External HTTP score 插件](../guide/cubemaster-scheduler-config#external-http-score-插件)。

## 边界

- 仅为演示 sidecar，不是生产打分服务。
- 在线创建 延迟须用客户端时间线 + sandbox Ready 测量，不能用 simulator 估计值代替。
- 运维演示不改变正式 binpack `VALID_NO_IMPROVEMENT`。
- **候选扇出：** `external_http_score` 会把 Filter 后的完整候选集（`selCtx.Nodes()`）POST 给 sidecar；集合大小由 `scheduler.pre_select_num` 控制（默认 `-1` = 不限制）。请求体、sidecar CPU，以及「每个已知候选都必须返回有限分」的契约都会随候选数放大。大集群启用前请限制 `pre_select_num`。没有负缓存；熔断**默认开启**（连续 `failure_threshold` 次失败后跳闸，默认 5），可用 `circuit_breaker.disable: true` 关闭。

## 相关

- Live 补充结论包（内部研究，英文）：[topic1-live-supplement-20260914](/dev/topic1-live-supplement-20260914/)
- Scheduler profile YAML 示例：[Scheduler Profile 配置示例](./scheduler-profile-config-example)
