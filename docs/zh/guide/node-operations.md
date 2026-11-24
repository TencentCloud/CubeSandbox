---
title: 节点相关操作
lang: zh-CN
---

# 节点相关操作

日常节点管理建议使用控制节点上的 `cubeopscli` 命令行工具，也可以使用 WebUI 节点详情页（12088 端口）。

本页说明 CubeSandbox 集群的主要计算节点操作：查看节点兼容性、管理机型与节点标签，以及增加、隔离、删除节点。

> cubeopscli 默认访问本地 127.0.0.1:3010 的 CubeOps 服务，对于 Terraform 集群版等分离部署的方案，可以使用 `--address` 和 `--port` 参数指定控制面地址。

可以通过以下命令确认节点 ID 和当前状态：

```bash
cubeopscli node list

# 查询单个节点
cubeopscli node list --hostid <node_id>
```

## 节点标识与动态 IP 解析

CubeSandbox 将计算节点的稳定身份标识（`node_id` / `InstanceID`）与可拨号地址（`HostIP` / Endpoint IP）解耦，使计算节点 IP 变更后控制面与数据面链路仍能自动跟随新 IP。

### `node_id` 与 Endpoint IP 的选择规则

`cubelet` 启动时按以下优先级解析自身身份（见 `Cubelet/pkg/utils/cloud.go`）：

1. **`node_id`（稳定节点标识）**：
   1. `CUBE_SANDBOX_NODE_ID`——最高优先级，可为任意稳定标识。在 Kubernetes Helm 部署中，Chart 通过 Downward API 默认注入 `spec.nodeName`，因此同一宿主机上 `cube-node` Pod 重启或重建后 `node_id` 保持不变。
   2. `CUBE_SANDBOX_NODE_IP`——当未设置 `CUBE_SANDBOX_NODE_ID` 时使用（一键安装 / systemd / Terraform 物理机部署默认走此路径，节点静态 IP 直接作为 `node_id`）。
   3. 自动探测的主网卡非回环 IPv4（默认路由网卡 → 出站探测 IPv4 → 首个非回环 IPv4）——以上两个环境变量均未设置时的兜底。
2. **Endpoint IP（可拨号的 `HostIP`）**：
   1. `CUBE_SANDBOX_ENDPOINT_IP`——在 Kubernetes Helm 部署中，Chart 通过 Downward API 默认注入 `status.podIP`。
   2. 若未设置，且 `node_id` 本身是合法的非回环 IPv4，则直接复用 `node_id`。
   3. 否则兜底使用自动探测的主网卡非回环 IPv4。

> **动态 IP 跟随的前提**：要使节点 IP 变更后控制面和数据面无感切换，`node_id` 在 IP 变更前后必须保持稳定（显式配置 `CUBE_SANDBOX_NODE_ID`，或使用 Kubernetes Helm Chart 默认注入的 `spec.nodeName`）。若未设置 `CUBE_SANDBOX_NODE_ID` 而由 IP 兜底作为 `node_id`，IP 变化会被视为一个新节点。

### 控制面与数据面如何跟随节点 IP 变更

当计算节点以相同 `node_id`、新 `HostIP` 重新注册时：

- **CubeOps**：更新节点注册信息中的 `HostIP`，并在注册与每次心跳时将 `node_id -> host_ip` 同步写入共享 Redis Hash `cube:v1:shared:node:endpoint`（节点删除时同步移除）；后续心跳保持最新注册的 `HostIP` 不被旧值覆盖。
- **CubeMaster**：在沙箱代理路由、本地缓存、暂停快照、异步任务和状态对账器中同时保存 `HostID` 与 `HostIP`，向 `cubelet` 发起调用前按 `node_id` 动态解析最新 `HostIP`；检测到节点 IP 变更时自动关闭旧 IP 对应的 gRPC 连接池。
- **CubeProxy**：在本地路由缓存中保存 `HostID`，通过 `cube:v1:shared:node:endpoint` 解析节点实时上游 IP（本地缓存 10s 刷新 TTL，并发刷新单飞合并，Redis 异常时优先回退到最近一次查到的实时 IP 并启用 1s 错误退避，无需清理存量沙箱路由缓存）。

**兼容性与滚动升级顺序**：

- 存量不含 `HostID` 的 `sandbox:proxy:{sandboxID}` 路由，在 CubeMaster 和 CubeProxy 中都会自动降级使用静态 `HostIP`，无需数据迁移。
- 推荐滚动升级顺序：先升级 **CubeOps**（节点心跳约 10s 内自动补齐 `cube:v1:shared:node:endpoint`），再升级 **CubeProxy** 和 **CubeMaster**（先升级 CubeMaster 再升级 CubeProxy 同样安全，旧版 CubeProxy 会忽略 `HostID` 并继续使用静态 `HostIP`）。
- **多副本 CubeMaster 滚动升级注意事项**：在所有 CubeMaster 副本完成升级前，请避免执行跨节点 Resume。新旧版本混跑期间，旧版 CubeMaster 不识别 `HostID`，跨节点恢复时仅改写 `HostIP` 而不会清除新版写入的 `HostID`，可能导致该沙箱继续解析到旧节点。
- **回滚注意事项**：若将 **CubeMaster** 从新版回滚到旧版，Redis 中已写入的 `HostID` 字段不会被旧版清理；后续旧版 CubeMaster 跨节点 Resume 时只改写 `HostIP`，而新版 CubeProxy 仍会按旧 `HostID` 解析并把流量导回原节点。回滚时请**先回滚 CubeProxy**，或清理受影响 `sandbox:proxy:{sandboxID}` 中的 `HostID` 字段。
- **CubeProxy 平滑重载说明**：升级后通过 nginx 平滑重载保留的旧路由缓存项不含 `HostID` 字段，首次命中时会集中从 Redis 重新加载一次。

## 节点分组

沙箱的跨机恢复要求源节点和目标节点的 CPU 和 Kernel 完全一致，对于这种机器我们称为同一机型。在一类机型上产生的内存快照只能恢复到同类机型上。

查看集群的节点类别及每组的成员：

```bash
cubeopscli node groups
```

输出示例（默认精简显示——组名、CPU 信息、成员数量和成员列表）：

```text
GROUP    CPU_VENDOR     CPU_MODEL             COUNT  NODES
type-1   GenuineIntel   Intel(R) Xeon(R) ...  3      node-1,node-2,node-3
type-2   AMD            AMD EPYC 7B13 ...     2      node-4,node-5(cordoned)
```

- 同组节点之间可以互相跨机恢复内存快照，不同组之间不可以。
- 节点状态标记：`(down)` 不健康、`(cordoned)` 已隔离（停止调度）、`(tainted)` 宿主机 KVM 模块带有可疑污染标志。
- `unknown` 组汇集了未上报 `cpuid_hash`/`host_kernel_release` 的节点（例如旧版本 Cubelet 未采集）。这类节点的类别无法确认，跨机恢复对其按失败处理。

在构建共享存储后端的模板、执行跨机恢复、规划宿主内核滚动升级之前，都可以先用该命令确认集群里混用了几种节点类型。

## 机型配置

机型用于声明一组可以互相恢复内存快照的节点。

默认的跨机恢复规则要求源、目标节点 CPU 相同**且**宿主内核版本完全一致。宿主内核滚动升级之后，同一批机器的内核小版本常常不一致，本可以互相恢复的机器因此被挡在门外。把这组机器绑定到同一个机型，即可豁免内核一致性要求：

- 在 WebUI 的「节点 → 机型管理」或用 `cubeopscli machine-type` 创建机型，并把节点绑定到机型上。
- 绑定后的节点会带上标签 `cube.cubesandbox.io/machine-type=<机型名>`；没有该标签就是未绑定，不需要逐节点开关。
- 机型只放宽内核比较，CPU 仍必须相同。

绑定机型后，跨机恢复的判定变为：

| 源节点 | 目标节点须满足 |
|---|---|
| 未绑定机型 | `cpuid_hash` 和 `host_kernel_release` 都相等（不变） |
| 绑定机型 `<T>` | 同为机型 `<T>` 且 `cpuid_hash` 相同；不再比较宿主内核 |

绑定机型后，节点只能恢复到同一机型、`cpuid_hash` 相同的节点；未绑定节点即使 CPU、内核一致也会被排除。给一组原本可互相恢复的节点启用机型时，要整组一起绑定。

一个机型里混入多种 CPU 时只会得到告警——这些节点之间依然无法跨机恢复。查看机型内的 CPU、内核分布用 `cubeopscli machine-type get <name>`。

删除仍绑定着节点的机型时必须同时解绑（CLI 加 `--unbind`）。

```bash
# 创建机型并绑定首批节点
cubeopscli machine-type create intel-oc9-k66 \
  --display-name "Intel OC9" \
  --description "same CPU, mixed host kernel" \
  --nodes node-1,node-2

# 查看机型：成员、CPU 与内核分布、告警
cubeopscli machine-type list
cubeopscli machine-type get intel-oc9-k66

# 之后随时绑定、解绑；bind 会把节点从原机型迁出
cubeopscli machine-type bind intel-oc9-k66 node-3
cubeopscli machine-type unbind node-3

# 按机型查找节点
cubeopscli node list --machine-type intel-oc9-k66
cubeopscli node list --unassigned-machine-type

# 删除仍绑定节点的机型需要 --unbind
cubeopscli machine-type delete intel-oc9-k66 --unbind
```

以上命令同样可以写成 `cubeopscli node machine-type …`。

## 节点标签

节点支持自由格式的标签，便于做自己的记录，例如 `team=infra`、`zone=gz`：

```bash
cubeopscli node label list node-1
cubeopscli node label set node-1 team=infra
cubeopscli node label replace node-1 team=infra zone=gz
cubeopscli node label delete node-1 team
```

`set` 增加或更新单个标签；`replace` 安装完整标签集，命令里没写的标签会被删除。系统标签——机型、隔离状态、Cubelet 上报的数据面标签（`kubernetes.io/*`、`beta.kubernetes.io/*`、`cube.cloud.tencentcloud.com/*`）——不会被标签命令改动。机型由上面的机型命令管理；隔离状态只能用 `node isolate` / `node unisolate` 修改。

## 增加节点

增加计算节点的过程是安装计算面组件，并让 Cubelet 向 CubeOps 注册。系统没有单独的 `node add` 命令；**计算节点服务启动后会自动完成注册**。

### 操作步骤

1. 将控制节点使用的同一份发布包复制到计算节点并解压。
2. 复制环境配置模板，然后设置计算节点角色、节点 IP 和控制面地址：

   ```bash
   cp env.example .env

   ONE_CLICK_DEPLOY_ROLE=compute
   CUBE_SANDBOX_NODE_IP=<当前节点IP>
   ONE_CLICK_CONTROL_PLANE_IP=<控制面IP>
   ```

3. （可选但强烈建议）从控制节点复制 `CUBE_S3_*` 到本机 `.env`：
   ```bash
   # 在控制节点执行，把输出拷贝到计算节点 .env
   grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
   ```
   也可指向其他共享的 S3 兼容存储。缺失时仅告警并继续安装，但 S3 卷插件不可用，安装结束时会再次提醒。
4. 安装计算节点：

   ```bash
   sudo ./install-compute.sh
   ```

5. 检查本地服务：

   ```bash
   sudo ./quickcheck.sh
   ```

6. 在控制节点确认新节点已经注册并处于健康状态：

   ```bash
   cubeopscli node list
   ```

7. 将需要的模板分发到新节点。根据模板状态和部署方式，可对目标节点执行 template redo，或者重新构建模板。

完整的环境变量、安装行为、调度配置和故障排查请参阅[多机集群部署](./multi-node-deploy.md)。

## 隔离节点

**隔离会临时阻止 CubeMaster 向指定计算节点调度新沙箱**，行为类似 Kubernetes `cordon`：节点仍可保持健康，已有沙箱继续运行。

对于隔离后的节点，后续的新沙箱调度不再选择该节点；如果没有其他可调度节点，创建会失败。而对于已有沙箱则不受影响，不会自动销毁或迁移。

::: warning 隔离不等于清空节点
隔离不会驱逐已有沙箱。执行会中断进程或网络的维护操作前，需要先隔离节点，再主动销毁该节点上的沙箱。
:::

### 隔离

```bash
# 隔离单个节点
cubeopscli node isolate <node_id>

# 隔离多个节点
cubeopscli node isolate <node_id_1> <node_id_2>
```

隔离操作是幂等的。执行成功后，确认 `SCHEDULING_DISABLED` 为 `true`：

```bash
cubeopscli node list
```

进行破坏性维护前，建议至少等待 60 秒，让正在进行的调度和创建窗口结束。

自动化场景可以使用带 JWT 的 CubeOps API：

```bash
curl -X PUT "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

### 取消隔离

维护完成后，允许节点重新接收新沙箱：

```bash
cubeopscli node unisolate <node_id>
```

也可以使用 API：

```bash
curl -X DELETE "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

节点重新投入使用前，确认 `SCHEDULING_DISABLED` 已变为 `false`。

Kubernetes 计算面升级时，请在隔离并清空节点后继续参考 [Kubernetes 升级](./kubernetes/upgrade.md)。

## 删除节点

删除节点会从 CubeOps 移除节点注册信息、状态、组件版本元数据和指标缓存。它不会停止节点进程、销毁沙箱、删除磁盘或删除 Kubernetes Node 对象。

::: warning 删除前必须隔离并清空节点
CubeOps 只允许删除已经隔离且没有沙箱的节点。删除操作本身不会隔离节点，也不会销毁节点上的负载。
:::

### 推荐顺序

1. 隔离目标节点。
2. 等待至少 60 秒，让正在进行的调度结束。
3. 销毁该节点上的全部沙箱。
4. 停止或退役 Cubelet，避免节点立即重新注册。
5. 删除节点记录。
6. 确认节点已从列表中消失。

### 使用 cubeopscli 删除

```bash
# 删除单个节点
cubeopscli node rm <node_id>

# rm 是 delete 的别名；支持一次删除多个节点
cubeopscli node delete <node_id_1> <node_id_2>
```

批量删除会依次尝试所有节点。某个节点失败不会阻止后续操作，命令结束时会返回错误并列出失败节点。

如果计算节点不可达，CubeOps 无法校验其沙箱清单，可以使用强制删除：

```bash
cubeopscli node rm --force <node_id>
```

`--force` 只跳过沙箱清单校验，目标节点仍然必须处于隔离状态。

### 使用内部 API 删除

```bash
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>"

# 节点不可达时跳过沙箱清单校验
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>?force=true"
```

常见错误包括：

- `404 Not Found`：节点不存在。
- `409 Conflict`：节点未隔离，或者仍有沙箱。
- `502 Bad Gateway`：CubeMaster 无法校验沙箱清单；请恢复连接或明确使用强制删除。

删除不是永久禁止注册。如果 Cubelet 仍在运行或再次启动，它可以使用相同节点 ID 重新注册为新节点记录。

## 相关文档

- [多机集群部署](./multi-node-deploy.md)
- [CubeMaster 调度器配置](./cubemaster-scheduler-config.md)
- [服务管理与日志](./service-management.md)
- [Kubernetes 升级](./kubernetes/upgrade.md)
