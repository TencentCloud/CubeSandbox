---
title: 节点相关操作
lang: zh-CN
---

# 节点相关操作

本页说明 CubeSandbox 集群的三类主要计算节点操作：增加节点、隔离节点和删除节点。

日常节点管理可以使用控制节点上的 `cubeopscli` 或 WebUI 节点详情页。以下示例默认 CubeOps 地址为 `127.0.0.1:3010`；多机部署时请替换为控制面 IP。

操作前后可以通过以下命令确认节点 ID 和当前状态：

```bash
cubeopscli node list

# 查询单个节点
cubeopscli node list --hostid <node_id>
```

## 增加节点

增加计算节点的过程是安装计算面组件，并让 Cubelet 向 CubeOps 注册。系统没有单独的 `node add` 命令；计算节点服务启动后会自动完成注册。

### 前置条件

- 已有使用相同 CubeSandbox 发布包安装的可用控制节点。
- 计算节点满足硬件和软件要求。
- 计算节点可以访问 CubeOps 的 `3010` 端口。
- 计算节点可以访问集群的 S3 兼容存储。使用内置 MinIO 时，需要能够访问控制节点的 `9000` 端口。

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

隔离会临时阻止 CubeMaster 向指定计算节点调度新沙箱，行为类似 Kubernetes `cordon`：节点仍可保持健康，已有沙箱继续运行。

| 维度 | 隔离后的行为 |
| --- | --- |
| 新沙箱调度 | 不再选择该节点；如果没有其他可调度节点，创建会失败。 |
| 已有沙箱 | 不受影响，不会自动销毁或迁移。 |
| 节点健康状态 | 与隔离相互独立，隔离节点仍可显示 `healthy=true`。 |
| Cubelet 心跳 | 继续正常，节点侧不能自行清除隔离状态。 |

CubeOps 使用以下保留 label 记录隔离状态：

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

普通 labels API 和 Cubelet 注册都不能设置或清除该 label。

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
