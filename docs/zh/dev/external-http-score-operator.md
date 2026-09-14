# External HTTP Score 运维最短路径

规范最短路径：仓库内 `CubeMaster/examples/external-http-score/VERIFY.md`。

本页是 docs 侧的产品化运维入口指针，不改变调度语义。字段说明见 [CubeMaster 调度配置](../guide/cubemaster-scheduler-config#external-http-score)。

## 边界

- 仅为演示 sidecar，不是生产打分服务。
- 在线创建 延迟须用客户端时间线 + sandbox Ready 测量，不能用 simulator 估计值代替。
- 运维演示不改变正式 binpack `VALID_NO_IMPROVEMENT`。

## 相关

- Live 补充结论包（英文）：[topic1-live-supplement-20260914](/dev/topic1-live-supplement-20260914/)
- Scheduler profile YAML 示例：[Scheduler Profile 配置示例](./scheduler-profile-config-example)
