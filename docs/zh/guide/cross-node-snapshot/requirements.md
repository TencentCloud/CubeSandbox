# 跨机迁移

沙箱的跨机迁移指把一个正在机器 A 上运行的沙箱，迁移到另一台机器 B 上继续运行，并尽可能保留沙箱原来的运行状态。

跨机迁移的价值主要有三个：

1. **资源调度。** 某台宿主机资源不足，可以将长时间运行的 Agent 沙箱迁移到另一台资源充足的机器，而不必重新执行任务。
2. **宿主机维护。** 当机器需要升级内核、重启或下线时，可以提前迁走沙箱，减少任务中断。
3. **负载均衡。** Agent 任务具有明显的动态特征，例如编译时 CPU 使用率很高，等待模型响应时资源使用率很低。跨机迁移可以帮助平台重新分配计算资源。

Cube 默认使用的 `xfs` 后端只在源节点保存 rootfs、memory 和 metadata。跨机迁移需要使用共享后端，即将源节点上的沙箱暂停或打快照后相关的数据上传到共享存储，派生后在目标节点恢复。目前支持两种共享后端：

- JuiceFS：部署方法见 [JuiceFS 服务配置](./juicefs.md)
- S3：部署方法见 [CubeS3lvol 服务配置](./cubes3lvol.md)

## 跨机迁移条件

并不是所有沙箱都支持跨机迁移，需要满足以下条件：

1. 沙箱使用共享后端的模板/快照创建；
2. 源节点和目的节点 CPU、kernel 和 KVM 兼容；
3. 源节点不可调度；

### 使用共享后端创建模板

使用 cubemastercli 或者 SDK API 默认创建的模板使用 `xfs` 后端，使用该后端的模板创建的沙箱不支持跨机迁移。

如果希望创建的沙箱支持跨机迁移，需要主动声明使用 `--backend jfs|s3` 创建支持跨机的模板。

```bash
# 创建 juicefs 模板
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --backend jfs
```

> 已创建的模板不支持在已有的 `xfs`、`jfs` 和 `s3` 产物之间转换。

### CPU、kernel 和 KVM 兼容

为了保证跨机后的沙箱能够正常恢复，需要保证源节点和目的节点 CPU、kernel 和 KVM 兼容。

可以使用 `cubeopscli node list --json` 查看节点的 `HostFacts` 信息。

| 字段 | 含义 | 当前门禁 |
|---|---|---|
| `cpuid_hash` | CPU 身份与特性哈希 | 相等匹配 |
| `host_kernel_release` | `uname -r` | 相等匹配 |
| `kvm_module_taint` | 强制、树外或未签名 KVM 模块标记 | 目标侧非空则拒绝 |
| `host_kernel_fingerprint` | kernel release 与规范化 cmdline 指纹 | 仅展示 |
| `cpu_vendor` | CPU 厂商 | 仅展示 |
| `kvm_api_version` | KVM API 版本 | 仅展示 |

`cpuid_hash` 是 Cubelet 对 `/proc/cpuinfo` 第一颗逻辑 CPU 的身份字段和排序后特性集计算的 SHA-256：x86 使用 `vendor_id`、family、model、stepping、flags；ARM 使用 implementer、architecture、variant、part、revision、Features。混合核心机器仍建议人工核对完整 `HostFacts`。

### 源节点不可调度

由于沙箱跨机需要将共享数据上传到目标节点，第一次在非本机节点恢复的性能较差，因此在源节点可用时调度器始终优先调度源节点。调度的优先级为：

1. 源节点健康且可调度时，在源节点恢复；
2. 源节点离线或被[隔离](../node-operations.md)时，检查共享数据和主机兼容性；
3. 条件全部满足后选择兼容节点，否则返回错误。

需要注意的是，外部存储也会限制调度：

- **host-mount**：如果沙箱使用了 host-mount 挂载的 Volume，则无法跨机迁移。因为需要无法保证挂载的目录在目标节点上也能访问，见 [host-mount 限制](../persistent-storage.md)
- **Plugin Volume**：如果沙箱使用了 Plugin Volume，则沙箱可以跨机，但需要保证目标节点上也安装了对应的 driver 并能访问后端。Cube 不会自动检查目标节点上的 Volume 状态以及是否可用，需要 Volumn 开发者自行保证，见 [Plugin Volume 限制](../volume-plugin.md)
