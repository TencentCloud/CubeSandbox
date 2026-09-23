# Agent 平台集成：手动 freeze / resume

面向在 CubeSandbox 之上暴露 `sandbox_freeze`、`sandbox_connect_by_id` 等工具的
**Agent 编排层**（不是终端用户 SDK 示例）。

## 心智模型：三层 S3 能力彼此独立

| 层 | 含义 | 保留什么 |
|----|------|----------|
| **Volume**（`cube-volume-s3` + MinIO） | 用户盘挂载，如 `/home/user/persistent` | **kill 后新建实例**仍保留的文件 |
| **s3lvol**（`ONE_CLICK_ENABLE_S3LVOL`） | 集群 snapshot 后端 | 跨节点 pause/snapshot |
| **模板 `--backend s3`** | 注册模板时指定 | 跨节点 pause/snapshot（`xfs` 模板也可同节点 pause；S3 用于跨节点，见 [跨机快照](cross-node-snapshot.md)） |

Volume **不能**替代实例 freeze。Freeze 保留**同一 sandboxId** 的内存+磁盘态；
Volume 在实例销毁后通过**新实例**保留文件。

## 关键：`pause()` 不会停止 idle 计时

默认 `on_timeout="kill"` 时，沙箱在创建时获得的 idle deadline **在 pause 后仍然有效**。
paused 沙箱仍可能在 deadline 到期时被 **销毁**。

**典型现象：** `POST /pause` 成功、CubeMaster 有 `action=pause` 日志，但一小时后
`connect` / `GET /sandboxes/{id}` 返回 404。

**缓解（可组合）：**

1. 用 `POST /sandboxes/{id}/timeout` 延长保留，请求体为 `{"timeout": <秒>}`（如 `{"timeout": 86400}`）。**running** 时应在 `pause` **之前**调用，或在 `pause` 返回后**立即**调用——idle sweeper 每隔数秒运行，两步之间的间隙仍按 pause 前的 deadline 计时。
2. **创建时**使用 `lifecycle.on_timeout="pause"` 并配合较长 `timeout`，或策略允许时使用 `NEVER_TIMEOUT`。
3. 恢复 paused 沙箱时，**先走管控面 `POST /sandboxes/{id}/connect`**，再连 envd 数据面（见下）。

## 推荐管控面流程

### Freeze（Agent 工具）

```text
1. POST /sandboxes/{id}/timeout  {"timeout": 86400}   # 仍在 running 时
2. POST /sandboxes/{id}/pause          # 等到 state=paused
3. 释放进程内句柄；在用户绑定存储中保留 sandboxId
```

第 3 步之前确认 pause 已落地：`GET /sandboxes/{id}` 应显示 `state="paused"`。无论直接调 REST 还是通过受支持的 SDK（含 E2B 兼容的 `sandbox.pause()`）均适用。

### Resume（connect_by_id）

```text
1. GET  /sandboxes/{id}                # 可选：检查 state
2. POST /sandboxes/{id}/connect        # paused 会自动 resume
3. POST /sandboxes/{id}/timeout {"timeout": 300}   # 可选：设置交互 idle 窗口
4. 连接 envd 数据面（命令、文件、VNC）
```

第 3 步在需要**更短** idle 窗口时必不可少：`connect(timeout=…)` 只会**延长**已有 deadline（例如 freeze 时设了 24h，`connect(timeout=300)` 仍约剩 24h）。在 connect 之后调用 `POST /timeout` 才能把交互窗口改为你想要的值。

对 **paused** 实例直接走 envd 或数据面常在管控面 resume 之前失败——常见为 stale 代理后端的**同节点 504**、pause 进行中的 **503 + Retry-After**，或沙箱已被销毁的 **410 Gone**；必须先经 `POST /sandboxes/{id}/connect`（见 [生命周期](lifecycle.md)）。

### Volume 权限（s3fs）

S3 volume 插件不会设置 s3fs 的 `uid`/`gid`/`umask`，guest 内可见的权限取决于 s3fs 默认值。
请使用官方支持的配置入口——安装器会生成并在升级时重写 `volume-s3.conf`，**不要手动编辑该文件**（见 [S3 Volume](./s3-volume.md)）：

```bash
# one-click（.one-click.env）
# install.sh 仅在变量为空时补上 -ouse_path_request_style；使用内置 MinIO 时需保留该 token。
CUBE_S3_S3FS_EXTRA_OPTS='-ouse_path_request_style -ouid=1000 -ogid=1000 -oumask=022'
```

```yaml
# Helm（values.yaml）——仅当 minio.enabled=false 且配置了 volumeS3.endpoint / existingSecret 时生效
volumeS3:
  extraOpts: "-ouse_path_request_style -ouid=1000 -ogid=1000 -oumask=022"
```

使用 chart 内置 MinIO 时，`volumeS3.extraOpts` 不会生效（选项由 chart 硬编码）。
自托管 MinIO 作为外部后端时同样需要 `-ouse_path_request_style`。

仅 [从零手动部署](./s3-volume.md#从零手动部署（非-one-click-非-helm）) 场景才在 `volume-s3.conf` 中设置 `S3FS_EXTRA_OPTS`。
也可在 Agent 启动流程或模板 entrypoint 中做挂载后 `chown`/`chmod`。
另见 [宿主机挂载权限](troubleshooting/host-mount-permissions.md) 中类似的属主排查思路。

## 运维检查清单

- [ ] 所有计算节点 s3lvol 就绪后再 `tpl create-from-image --backend s3`
  （Kubernetes 上启用 s3lvol 会重建 Big Pod，并在 **Pod 网络**（`cubeNode.hostNetwork: false`）下中断该节点上的沙箱；见 [Kubernetes 升级](./kubernetes/upgrade.md)）
- [ ] 所有计算节点上 S3 模板均为 **READY**
  （单节点 FAILED 时用 `cubemastercli tpl redo --template-id <id> --failed-only --node <node-ip>`）
- [ ] 用户 Volume 桶与 s3lvol 快照桶分离
- [ ] Agent 绑定 TTL ≤ paused 保留 timeout（避免绑定到已销毁 ID）

## 延伸阅读

- [生命周期](lifecycle.md) — `timeout`、`on_timeout`、`connect(timeout=…)`
- [跨机快照](cross-node-snapshot.md)
- [S3 Volume](./s3-volume.md)
