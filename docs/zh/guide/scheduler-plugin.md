# 可扩展调度插件

CubeMaster 支持按请求场景选择调度 Profile。每个 Profile 由不可关闭的安全 Guards、可选 Filter、带权 Score、选点方式和失败策略组成。

二进制内置三条出厂 Profile（`CubeMaster/pkg/base/config/scheduler_factory.yaml`）：`burst_balance`、`template_reuse` 通过请求 label `workload=burst_balance` / `workload=template_reuse` 选中，其余请求落入默认的 `mixed_binpack`。当配置中既没有 `scheduler.profiles` 也没有 legacy 的 `scheduler.filter` / `scheduler.score` 时，系统自动注入这套出厂策略，零配置部署也能按真实策略调度。一旦显式配置了 `scheduler.profiles` 或 legacy filter/score 中的任意一项，出厂策略整体不生效；仅配置 legacy 项时，系统仍把 `filter`、`score`、`postscore` 和 `priority_select_num` 编译为兼容的 `default` Profile，保持原有行为。

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

`selection.method` 决定如何从评分结果中选出最终节点：`highest` 严格选取评分最高的节点；`spread` 在评分最高的前 `top_n` 个候选中确定性选取当前运行沙箱数最少的节点（占用相同时保持评分顺序），用于把放置摊开；`random`（缺省）在前 `top_n` 个候选中按分数加权随机。`top_n: -1` 表示候选范围为全部通过过滤的节点。

自定义 Profile 固定执行 `node_safety`、`cpu`、`mem`、`disk`、`template_locality` 和 `realtime_create_num` Guards，配置不能关闭或重复声明这些安全约束。其中 `node_safety` 会在正常路径和 backoff 路径检查健康度、指标新鲜度、MVM 上限及 CPU load 合法性。

选定节点后，CubeMaster 会重读该节点并原子预留本次请求的 CPU、内存配额、一个 MVM 槽位和一个创建并发槽位，避免并发创建在节点指标更新前反复落到同一节点。预留冲突会换节点有限重选；Cubelet 创建调用返回后（无论成功或失败）即释放预留，成功场景由下一次节点指标上报接管记账。多 CubeMaster 副本通过按节点的 Redis 计数协调预留，Redis 不可用时退化为本地预留。本副本持有的在途预留数以 `node.reserved` 暴露给插件。

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

协议位于 `pkgs/proto/services/schedulerplugin/v1/plugin.proto`。调用顺序为 `Handshake`、`SyncSnapshot`，再批量调用 `Filter` 或 `Score`。每次调度尝试都会生成新的快照版本，且并发请求会交错调用，因此插件服务端必须按 `snapshot_version` 键存快照（并设置淘汰上限），不能只保留单个"最新"槽位。生产环境建议使用 Unix Domain Socket。可运行示例位于 `CubeMaster/examples/scheduler-plugin`：

```bash
cd CubeMaster
SOCKET=/tmp/cube-scheduler-example.sock go run ./examples/scheduler-plugin
```

## 失败语义

- Mandatory Guard 始终 fail-closed。
- Filter 默认 `fail-closed`；`fail-open` 必须显式配置，并会输出风险告警。
- Score 默认 `default-score`，单个插件失败后用其 `default_score` 继续；也可配置 `fail-closed`。
- `no_candidate` 支持 `fail` 和 `backoff`。自定义 Profile 使用 backoff 时仍会重新执行 Guards、Filter 和 Score。

配置在启动或热更新时整体编译；插件名、路由、表达式、权重、选点方式或失败策略无效时，新 Profile 集不会生效，调度器继续使用上一份完整管线。
