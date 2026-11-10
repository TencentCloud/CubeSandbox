# 计算节点自动扩缩

CubeOps 可以根据 Cube 调度器的额度来决定计算节点池的大小，由 Kubernetes
Cluster Autoscaler 实际增删机器（[#2193](https://github.com/TencentCloud/CubeSandbox/issues/2193)）。默认关闭。

## 为什么需要这一层

Cube MicroVM 运行在 Cubelet 内部，不是 Kubernetes Pod。Cluster Autoscaler
看不到 sandbox 的需求，会把每台计算节点都当成空节点：调小节点池时会随机删掉
节点，连同上面正在运行的 sandbox；在 Cube 删除这些节点的元数据之前，查询这些
sandbox 都会出错。

## 工作方式

CubeOps 在每台要保留的计算节点上放一个很小的**占位 Pod**。节点上有不属于任何
控制器的 Pod 时，Cluster Autoscaler 不会删除它，所以占位 Pod 能把节点固定住。

- **扩容**：调度额度用量加预留余量超过可用节点的目标水位时，CubeOps 再建一个
  占位 Pod。它在现有节点上放不下（每节点一个），处于 Pending，Cluster
  Autoscaler 就会加一台机器。新节点在 Cube 注册并上报健康后才计入容量；
  已有节点在启动时不会重复请求。
- **缩容**：需求持续低于节点池超过 `scaleInCooldown` 后，CubeOps 隔离 MicroVM
  最少的节点，等到上面没有运行中和暂停的 sandbox，再次确认隔离状态，然后才
  删除这台节点的占位 Pod，Cluster Autoscaler 删除的正是这台机器。机器删除后，
  CubeOps 删除 Cube 中的节点元数据。不会停止任何 sandbox：超过 `drainTimeout`
  仍未清空的节点会恢复服务。
- 一次只下线一台。运维手动隔离的节点不会被处理，并保持固定。
- 所有状态都从 Cube 节点元数据、节点操作记录和占位 Pod 推导，CubeOps 重启后
  会接着完成；多副本通过数据库锁协调。

CubeOps 的 `GET /internal/v1/autoscale/status` 返回最近一次的决策：需求、各节点
状态、计划动作和告警。

## 启用

1. 确认计算节点池已开启 Cluster Autoscaler，上下限覆盖 `minNodes..maxNodes`，
   但**先不要调低下限**。
2. 打开自动扩缩，等到每台计算节点都有占位 Pod
   （`kubectl get pods -l cube.cloud.tencentcloud.com/capacity-placeholder=true -o wide`）：

   ```yaml
   cubeOps:
     autoscale:
       enabled: true
       nodeSelector:
         nodepool: cube-compute          # 计算节点池的节点标签
       tolerations:
         - key: cube.tencent.com/compute
           operator: Equal
           value: "true"
           effect: NoSchedule
       minNodes: 1
       maxNodes: 10
       targetUtilization: 0.7
   ```

3. 调低节点池下限。此后只有清空的节点会被删除。

可以先设 `dryRun: true`，只在 CubeOps 日志里看决策，不做任何改动。

## 限制

- 容量按调度额度（`quota_cpu`、`quota_mem_mb`）和当前用量计算；因容量不足被
  拒绝的创建请求暂未作为输入。
- 有长期暂停 sandbox 的节点无法清空，会阻塞缩容直到 `drainTimeout`，之后恢复
  服务。
- 本版本不包含 sandbox 迁移、碎片整理和云厂商中断处理。
- Kubernetes 节点名必须等于 Cube 节点 ID（chart 默认如此）。
