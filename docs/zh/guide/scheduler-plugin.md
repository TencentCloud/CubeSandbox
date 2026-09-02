# 可扩展调度插件

CubeMaster 支持按请求场景选择调度 Profile。每个 Profile 由不可关闭的安全 Guards、可选 Filter、带权 Score、选点方式和失败策略组成。未配置 `profiles` 时，系统把原有 `filter`、`score`、`postscore` 和 `priority_select_num` 编译为 `default` Profile，保持原有行为。

## Profile 配置

只有 `profile_route_label_keys` 中列出的请求 label 可以参与路由，也只有这些 label 会传给外部插件。非默认 Profile 必须包含 instance type 或 label 条件；路由按配置顺序匹配，第一个命中的 Profile 生效。

```yaml
scheduler:
  profile_route_label_keys: [workload]
  profiles:
    - name: burst
      route:
        instance_types: ["S.*", "M.*"]
        labels: {workload: burst}
      filters:
        - name: skip-high-create
          type: expr
          expr: "node.creating + node.reserved < 8"
      scores:
        - name: prefer-idle
          type: expr
          expr: "node.cpu_util < 60.0 ? 80.0 : 20.0"
          weight: 2
      selection: {top_n: 5, method: spread}
      failure:
        filter: fail-closed
        score: default-score
        no_candidate: fail
```

自定义 Profile 固定执行 `node_safety`、`cpu`、`mem`、`disk`、`template_locality` 和 `realtime_create_num` Guards，配置不能关闭或重复声明这些安全约束。其中 `node_safety` 会在正常路径和 backoff 路径检查健康度、指标新鲜度、MVM 上限及 CPU load 合法性。

`selection.method` 支持 `random`、`spread` 和 `highest`。目前 `spread` 与 `random` 等价：都从得分最高的前 `top_n` 个节点中均匀随机选点，只有 `highest` 始终选择得分最高的节点。自定义 Profile 未显式配置 `top_n` 时默认为 1，此时 `random`/`spread` 等价于确定性地选择最优节点；而 legacy `default` Profile 使用 `priority_select_num`。

基于 label 的路由只在创建沙箱（create）路径生效。迁移（migrate）和恢复放置（restore placement）调度不会设置路由 label，因此按 label 路由的 Profile 在这两条路径上不可达，相关请求始终走 fallback 管线。

## 插件类型

- `go`（默认）：编译进 CubeMaster，通过统一 Registry 按名称注册。
- `expr`：启动时编译 CEL；Filter 必须返回 `bool`，Score 必须返回 0—100 的数值。
- `grpc`：连接独立进程，启动时完成协议/能力握手；请求超时、连续失败熔断、快照版本及返回节点/分数均由 CubeMaster 校验。

进程内 Go 插件实现现有 `filter.Selector` 或 `score.Selector` 接口，并在包初始化时调用 `plugin.RegisterGoFilter` / `plugin.RegisterGoScore`。CubeMaster 二进制需导入该包，因此新增 Go 插件后需要重新编译；重复名称会在启动时被拒绝。

CEL 提供基于版本化 protobuf 的强类型只读对象 `node` 与 `request`，未知字段、错误类型运算和不合法返回类型会在 Profile 激活时被拒绝。常用节点字段包括 `cpu_util`、`cpu_load`、`quota_cpu`、`allocated_cpu`、`quota_mem_mb`、`allocated_mem_mb`、`creating`、`local_creating`、`reserved`、`mvm_num`、`labels`、`local_templates`、`template_local` 和 `snapshot_storage_writable`；请求字段包括 `instance_type`、`cpu_millis`、`memory_bytes`、`system_disk_size`、`template_id` 和 `labels`。

外部插件配置示例：

```yaml
      filters:
        - name: company-policy
          type: grpc
          socket_path: /run/cube/company-scheduler.sock
          timeout: 100ms
          circuit_breaker_failures: 3
          circuit_breaker_cooldown: 30s
```

协议位于 `pkgs/proto/services/schedulerplugin/v1/plugin.proto`。调用顺序为 `Handshake`、`SyncSnapshot`，再批量调用 `Filter` 或 `Score`。生产环境建议使用 Unix Domain Socket。可运行示例位于 `CubeMaster/examples/scheduler-plugin`：

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## 失败语义

- Mandatory Guard 始终 fail-closed。
- Filter 默认 `fail-closed`；`fail-open` 必须显式配置，并会输出风险告警。
- Score 默认 `default-score`，单个插件失败后用其 `default_score` 继续；也可配置 `fail-closed`。
- 返回空结果的内置 `go` Score（例如请求没有节点偏好亲和性时的 `affinity_score`，或资源权重不适用时的 `image_score`）视为「不适用」并被跳过——不报错，也不替换为 `default_score`。只覆盖部分候选节点的结果仍视为失败。
- `no_candidate` 支持 `fail` 和 `backoff`。自定义 Profile 使用 backoff 时仍会重新执行 Guards、Filter 和 Score。

配置在启动或热更新时整体编译；插件名、路由、表达式、权重、选点方式或失败策略无效时，新 Profile 集不会生效，调度器继续使用上一份完整管线。

## 兼容性说明

- 未注册为插件的 legacy `enable_filters` / `enable_scorers` 配置项现在会使 CubeMaster 启动失败，错误信息会指出具体配置项；此前这类条目会被静默跳过。升级前请从配置中移除失效条目，或先注册对应插件。
