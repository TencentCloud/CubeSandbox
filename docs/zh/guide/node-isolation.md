---
title: 隔离节点
lang: zh-CN
---

# 隔离节点

节点隔离（isolate）用于在维护、升级或排障时，**临时阻止 CubeMaster 向指定计算节点调度新沙箱**。它类似 Kubernetes 的 `cordon`：节点仍可保持健康、已有沙箱继续运行，只是不再接收新负载。

::: tip 当前入口
WebUI / CubeOps / 对外 OpenAPI **暂不提供**隔离操作。请通过 **CubeMaster HTTP 接口**，或控制节点上的 **`cubemastercli`** 完成隔离与取消隔离。
:::

## 读完本页你会知道

- 隔离与「下线 / 驱逐」的区别
- 如何查找 `node_id` 并隔离 / 取消隔离
- 如何确认隔离是否生效
- 升级、维护等常见场景下的推荐操作顺序

## 行为说明

| 维度 | 隔离后的表现 |
|---|---|
| **新沙箱调度** | 该节点不再被选中；若集群中没有其他可调度节点，创建会失败 |
| **已有沙箱** | **不受影响**，不会自动销毁或迁移 |
| **节点健康状态** | 与隔离正交：隔离节点仍可显示为 `healthy=true` |
| **Cubelet 心跳 / 注册** | 继续正常；节点侧无法自行覆盖或清除隔离标记 |

内部实现上，CubeMaster 会在节点元数据中写入保留 label：

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

该 label **不能**通过普通 labels API 或 Cubelet 注册伪造 / 清除，只能走本文的隔离 / 取消隔离接口。

::: warning 隔离 ≠ 清空节点
隔离**不会**驱逐存量沙箱。若你要做会中断沙箱网络或进程的操作（例如 K8s 计算面升级会 recreate Big Pod），需要在隔离之后**自行销毁**该节点上的沙箱，再进行维护。详见 [K8s 升级指南](./kubernetes/upgrade.md)。
:::

## 前置条件

- 能访问控制节点上的 CubeMaster（默认 HTTP 端口 **8089**）
- 目标节点已在 CubeMaster 完成注册（`node_id` 存在）
- 若使用 CLI：控制节点上已安装 `cubemastercli`，并可连通 CubeMaster

下文示例默认在控制节点本机执行（`127.0.0.1:8089`）。多机部署时，把地址换成控制节点 IP 即可。

## 查找节点 ID

先列出集群节点，确认要操作的 `NODE_ID` 与当前隔离状态：

```bash
# CLI
cubemastercli --address 127.0.0.1 --port 8089 node list

# 或直接调接口
curl -s http://127.0.0.1:8089/internal/meta/nodes | jq .
```

CLI 输出中关注 `SCHEDULING_DISABLED` 列：`true` 表示已隔离。

也可以查询单个节点：

```bash
curl -s http://127.0.0.1:8089/internal/meta/nodes/<node_id> | jq '{
  node_id,
  host_ip,
  healthy,
  scheduling_disabled,
  labels
}'
```

## 隔离节点

### 方式一：HTTP 接口（推荐脚本 / 自动化）

```bash
curl -X PUT "http://127.0.0.1:8089/internal/meta/nodes/<node_id>/isolation"
```

成功时返回类似：

```json
{
  "ret": {
    "RetCode": 200,
    "RetMsg": "Success"
  },
  "data": {
    "node_id": "node-1",
    "host_ip": "10.0.0.1",
    "healthy": true,
    "scheduling_disabled": true,
    "labels": {
      "cube.cloud.tencentcloud.com/scheduling-disabled": "true"
    }
  }
}
```

接口**幂等**：对已隔离节点重复 `PUT` 是安全的。无需请求体。

### 方式二：cubemastercli

```bash
# 隔离单个节点
cubemastercli --address 127.0.0.1 --port 8089 node isolate <node_id>

# 一次隔离多个节点
cubemastercli --address 127.0.0.1 --port 8089 node isolate <node_id_1> <node_id_2>

# 需要原始 JSON 时
cubemastercli --address 127.0.0.1 --port 8089 node isolate --json <node_id>
```

成功时 CLI 会打印类似：

```text
node node-1 isolated: scheduling_disabled=true
```

## 确认隔离生效

再次查询节点，确认 `scheduling_disabled` 为 `true`：

```bash
curl -s http://127.0.0.1:8089/internal/meta/nodes/<node_id> | jq '.scheduling_disabled'
# 期望输出: true

cubemastercli --address 127.0.0.1 --port 8089 node list
# SCHEDULING_DISABLED 列应为 true
```

::: tip 建议等待窗口
隔离后建议再等待 **≥ 60 秒**，让进行中的调度 / 创建窗口结束，再对该节点做破坏性维护（重启、升级、下线等）。
:::

## 取消隔离

维护完成后，取消隔离，节点即可重新接收新沙箱：

```bash
# HTTP
curl -X DELETE "http://127.0.0.1:8089/internal/meta/nodes/<node_id>/isolation"

# CLI
cubemastercli --address 127.0.0.1 --port 8089 node unisolate <node_id>
```

成功后 `scheduling_disabled` 应为 `false`，且 labels 中不再包含 `scheduling-disabled`。

## 典型场景

### 节点维护 / 重启前

1. 隔离目标节点
2. 等待 ≥ 60 秒
3. （按需要）销毁该节点上的存量沙箱
4. 执行维护或重启
5. 节点恢复并重新注册后，取消隔离

### K8s 计算面升级（会 recreate Big Pod）

计算面升级会中断该节点上的存量沙箱网络。推荐顺序：

1. 调用 isolate API 隔离节点
2. 等待 ≥ 60 秒
3. **销毁**该节点上的沙箱
4. 再执行升级

完整步骤见 [K8s 升级指南](./kubernetes/upgrade.md)。

## 范围与限制

- **不是 drain**：不会自动迁移或销毁已有沙箱。
- **单节点 / 全部隔离**：若集群中没有其它可调度节点，新沙箱创建会失败（调度选不到节点）。
- **与健康检查正交**：隔离节点仍可保持 Healthy，仍会出现在健康节点列表中，只是不进入可调度集合。
- **与 Kubernetes `kubectl cordon` 无关**：本能力只影响 CubeMaster 调度，不会自动对 K8s Node 执行 cordon。
- **鉴权**：CubeMaster 默认关闭 HTTP 鉴权时可直接调用。若你开启了 CubeMaster 鉴权，需按集群配置补齐签名头；当前 `cubemastercli node isolate/unisolate` **不会**自动附加签名，鉴权开启时请优先使用带签的 HTTP 请求。

## 相关文档

- [服务管理与日志](./service-management.md) — 控制面 / 计算面服务启停与日志
- [K8s 升级](./kubernetes/upgrade.md) — 升级前隔离节点并清空沙箱
- [多机部署](./multi-node-deploy.md) — 节点注册与 `/internal/meta/nodes` 验收
