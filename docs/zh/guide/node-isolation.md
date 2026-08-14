---
title: 隔离节点
lang: zh-CN
---

# 隔离节点

节点隔离（isolate）用于在维护、升级或排障时，**临时阻止 CubeMaster 向指定计算节点调度新沙箱**。它类似 Kubernetes 的 `cordon`：节点仍可保持健康、已有沙箱继续运行，只是不再接收新负载。

::: tip 当前入口
使用控制节点上的 **`cubeopscli`**，或通过 **WebUI** 节点详情页完成隔离与取消隔离。
::::

## 读完本页你会知道

- 隔离与「下线 / 驱逐」的区别
- 如何查找 `node_id` 并隔离 / 取消隔离
- 如何安全地删除一个已退役节点的控制面记录
- 如何确认隔离是否生效
- 升级、维护等常见场景下的推荐操作顺序

## 行为说明

| 维度 | 隔离后的表现 |
|---|---|
| **新沙箱调度** | 该节点不再被选中；若集群中没有其他可调度节点，创建会失败 |
| **已有沙箱** | **不受影响**，不会自动销毁或迁移 |
| **节点健康状态** | 与隔离正交：隔离节点仍可显示为 `healthy=true` |
| **Cubelet 心跳 / 注册** | 继续正常；节点侧无法自行覆盖或清除隔离标记 |

内部实现上，CubeOps 会在节点元数据中写入保留 label：

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

该 label **不能**通过普通 labels API 或 Cubelet 注册伪造 / 清除，只能走本文的隔离 / 取消隔离接口。

:::: warning 隔离 ≠ 清空节点
隔离**不会**驱逐存量沙箱。若你要做会中断沙箱网络或进程的操作（例如 K8s 计算面升级会 recreate Big Pod），需要在隔离之后**自行销毁**该节点上的沙箱，再进行维护。详见 [K8s 升级指南](./kubernetes/upgrade.md)。
::::

## 前置条件

- 能访问控制节点上的 CubeOps（默认 HTTP 端口 **3010**）
- 目标节点已在 CubeOps 完成注册（`node_id` 存在）
- 若使用 CLI：控制节点上已安装 `cubeopscli`，并可连通 CubeOps

下文示例默认在控制节点本机执行（`127.0.0.1:3010`）。多机部署时，把地址换成控制节点 IP 即可。

## 查找节点 ID

先列出集群节点，确认要操作的 `NODE_ID` 与当前隔离状态：

```bash
# CLI
cubeopscli --address 127.0.0.1 --port 3010 node list

# 或直接调接口（需 JWT 鉴权）
TOKEN=$(curl -s http://127.0.0.1:3010/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"<用户名>","password":"<密码>"}' | jq -r '.accessToken')
curl -s http://127.0.0.1:3010/api/v1/nodes \
  -H "Authorization: Bearer $TOKEN" | jq .
```

CLI 输出中关注 `SCHEDULING_DISABLED` 列：`true` 表示已隔离。

也可以查询单个节点：

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list --hostid <node_id>
```

## 隔离节点

### 方式一：cubeopscli

```bash
# 隔离单个节点
cubeopscli --address 127.0.0.1 --port 3010 node isolate <node_id>

# 一次隔离多个节点
cubeopscli --address 127.0.0.1 --port 3010 node isolate <node_id_1> <node_id_2>

# 需要原始 JSON 时
cubeopscli --address 127.0.0.1 --port 3010 node isolate --json <node_id>
```

成功时 CLI 会打印类似：

```text
node node-1 isolated: scheduling_disabled=true
```

调用**幂等**：对已隔离节点重复隔离是安全的。

### 方式二：HTTP 接口（推荐脚本 / 自动化）

```bash
curl -X PUT "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

成功时返回类似：

```json
{
  "node_id": "node-1",
  "host_ip": "10.0.0.1",
  "healthy": true,
  "scheduling_disabled": true,
  "labels": {
    "cube.cloud.tencentcloud.com/scheduling-disabled": "true"
  }
}
```

无需请求体。

## 确认隔离生效

再次查询节点，确认 `scheduling_disabled` 为 `true`：

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list
# SCHEDULING_DISABLED 列应为 true
```

:::: tip 建议等待窗口
隔离后建议再等待 **≥ 60 秒**，让进行中的调度 / 创建窗口结束，再对该节点做破坏性维护（重启、升级、下线等）。
::::

## 取消隔离

维护完成后，取消隔离，节点即可重新接收新沙箱：

```bash
# CLI
cubeopscli --address 127.0.0.1 --port 3010 node unisolate <node_id>

# HTTP
curl -X DELETE "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

成功后 `scheduling_disabled` 应为 `false`，且 labels 中不再包含 `scheduling-disabled`。

## 删除节点

节点删除会从 CubeOps 移除一个已退役节点的控制面记录：删除节点的注册信息、状态、组件版本元数据，并清理该节点在 Redis 中的 metric 缓存。它**不会触及计算节点上的进程、沙箱、磁盘或 Kubernetes Node 对象**。

::: warning 删除前必须先隔离并清空节点
CubeOps 只允许删除**已隔离且无沙箱**的节点。删除操作本身不会隔离节点，也不会销毁或迁移沙箱。请先隔离节点、等待正在进行的创建完成，再手动销毁该节点上的所有沙箱。

仅当计算节点当前不可达但必须删除时，才使用强制删除。强制删除会跳过沙箱清单校验，但**仍要求先隔离**。
:::

### 推荐顺序

1. 隔离目标节点。
2. 等待 ≥ 60 秒，让在途的调度 / 创建窗口结束。
3. 销毁该节点上的全部沙箱。
4. 删除节点。
5. 停止或退役 Cubelet。
6. 确认节点列表中已无该记录。

若计算节点不可达，普通删除会因为无法通过 CubeMaster 校验沙箱清单而失败，此时可使用强制删除。

### 方式一（推荐）：cubeopscli

```bash
# 删除单个节点
cubeopscli --address 127.0.0.1 --port 3010 node rm <node_id>

# rm 是 delete 的别名；也支持一次删除多个节点
cubeopscli --address 127.0.0.1 --port 3010 node delete <node_id_1> <node_id_2>

# 强制删除（跳过沙箱清单校验）
cubeopscli --address 127.0.0.1 --port 3010 node rm --force <node_id>
```

批量删除会逐个处理：某个节点失败不会中断后续节点，但命令最终会返回非零退出码并列出失败的节点。若沙箱清单校验失败，错误信息会提示恢复 CubeMaster 连通性或显式使用 `--force` 重试。删除成功时 CLI 输出：

```text
node node-1 deleted
```

### 方式二：HTTP 接口（内部）

```bash
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>"

# 当无法校验在线沙箱清单时，强制删除
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>?force=true"
```

响应体是被删除节点的 JSON 快照。常见失败状态码：

- `404 Not Found` —— 节点不存在。
- `409 Conflict` —— 节点未隔离，或仍持有沙箱。
- `502 Bad Gateway` —— 无法连接 CubeMaster 校验沙箱清单；恢复连通性后重试，或使用 `force=true`。

`force=true` 仅跳过沙箱清单校验，**不会跳过隔离校验**。

### 删除之后

- 节点会立即从当前 CubeOps 实例消失；其他 CubeOps 实例会在下一次 metadata reload 时移除该节点。
- 删除不是永久注销。运行中或重启的 Cubelet 仍可使用同一个 `node_id` 重新注册。
- 要让节点重新上线，等待它重新注册并确认健康即可。新注册不会保留之前的隔离标记。

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

## 相关文档

- [服务管理与日志](./service-management.md) — 控制面 / 计算面服务启停与日志
- [K8s 升级](./kubernetes/upgrade.md) — 升级前隔离节点并清空沙箱
- [多机部署](./multi-node-deploy.md) — 节点注册与 `/api/v1/nodes` 验收
