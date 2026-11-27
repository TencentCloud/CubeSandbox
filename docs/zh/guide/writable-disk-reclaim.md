# 可写磁盘回收

可写磁盘回收会在沙箱删除文件后，释放可写根盘背后不再使用的存储空间。它适合长期运行、频繁创建和删除临时文件、构建产物或缓存的沙箱。

该功能默认关闭，在制作模板时开启，由这个模板创建的沙箱统一使用该设置。它只作用于可写根盘，只读镜像和挂载的数据卷保持原有行为。

回收会增加存储 I/O，删除文件或同步写入时可能花费更多时间，远端存储上尤其需要关注。如果减少存储占用带来的收益值得这部分开销，可以按模板开启；生命周期短、文件变化少的沙箱可以保持默认设置。

## 选择存储后端

Guest 的可写根文件系统需要使用 ext4。目标节点需要使用支持可写磁盘回收的运行时，并配置好对应的存储后端。

| 模板后端 | 使用条件 | 空间如何释放 |
| --- | --- | --- |
| `xfs` | 宿主机上的可写镜像存放在 XFS 文件系统中。 | 对镜像中不再使用的区间打洞，将没有共享引用的数据块归还宿主机文件系统。 |
| `jfs` | 已配置 [JuiceFS 服务](./cross-node-snapshot/juicefs.md)。 | JuiceFS 撤销文件映射，再由后台合并和清理释放没有引用的数据，对象存储占用随后下降。 |
| `s3` | 已配置 [CubeS3lvol 服务](./cross-node-snapshot/cubes3lvol.md)，且块设备支持 discard。 | CubeS3lvol 处理 discard 请求，按后端存储策略释放底层 chunk 和对象。 |

开启回收后，如果节点无法提供所需的存储能力，沙箱创建会失败。请先准备好存储服务并升级目标节点的运行时，再制作开启回收的模板。

## 制作模板时开启回收

下面使用 `xfs` 后端，以及预装了 `envd`、监听 `49983` 端口的基础镜像。已配置 JuiceFS 或 CubeS3lvol 的部署可以将后端改为 `jfs` 或 `s3`。

### CLI

在模板构建命令中加入 `--writable-disk-reclaim`：

```bash
cubemastercli tpl create-from-image \
  --image ghcr.io/tencentcloud/cubesandbox-base:latest \
  --backend xfs \
  --writable-layer-size 2G \
  --writable-disk-reclaim \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

使用返回的构建任务 ID 等待模板就绪：

```bash
cubemastercli tpl watch --job-id <job_id>
```

模板达到 `READY` 后，照常使用模板 ID 创建沙箱即可。完整的制作流程见[从 OCI 镜像制作模板](./tutorials/template-from-image.md)。

### API

调用 `POST /templates` 时，将 `writableDiskReclaim` 设为 `true`：

```json
{
  "image": "ghcr.io/tencentcloud/cubesandbox-base:latest",
  "backend": "xfs",
  "writableLayerSize": "2G",
  "writableDiskReclaim": true,
  "exposedPorts": [49983],
  "probePort": 49983,
  "probePath": "/health"
}
```

省略该字段或将其设为 `false`，即可制作关闭回收的模板。

### SDK

在模板构建参数中使用对应字段：

| SDK | 参数 |
| --- | --- |
| [Python](./tutorials/sdk/python.md) | `Template.build(..., writable_disk_reclaim=True)` |
| [Node.js](./tutorials/sdk/nodejs.md) | `Template.build({ ..., writableDiskReclaim: true })` |
| [Go](./tutorials/sdk/go.md) | `BuildTemplateOptions.WritableDiskReclaim`，传入指向 `true` 的 `*bool` |

例如，使用 Go SDK 时，在传给 `client.BuildTemplate` 的配置中加入该字段：

```go
enabled := true
opts := cubesandbox.BuildTemplateOptions{
    Image:               "ghcr.io/tencentcloud/cubesandbox-base:latest",
    WritableLayerSize:   "2G",
    WritableDiskReclaim: &enabled,
    // 保留模板原有的端口、探针及其他配置。
}
```

Go 中的 `nil` 表示省略该选项，指向 `false` 的指针表示显式关闭。

## 使用和调整策略

沙箱启动后，正常删除文件就会通过 Guest 文件系统触发回收，无需额外调用回收 API，也无需配置定时 trim 任务。

回收策略随模板固定，创建沙箱时不能覆盖，重建已有模板也会沿用原策略。如果希望为某个工作负载开启或关闭回收，请按所需设置制作新模板，再使用新模板创建沙箱。

暂停恢复和基于快照创建克隆都会保留磁盘的回收设置。相关操作见[快照、回滚与克隆](./snapshot-rollback-clone.md)。

## 理解存储占用

回收改变的是底层实际分配的空间，不改变磁盘的配置容量。可写盘配置为 `2G`，回收后容量仍然是 `2G`；释放的底层空间由存储后端重新利用。

如果数据仍被快照或克隆引用，要等这些引用释放后才能归还空间。JuiceFS 和 CubeS3lvol 的对象存储清理也可能晚于文件删除完成，因此占用下降可能有延迟。

评估收益时，应查看实际希望节省的存储：XFS 上看镜像的已分配空间，JuiceFS 和 CubeS3lvol 上看底层对象占用。Guest 报告的空闲空间和宿主机镜像文件的逻辑长度，都不能直接表示已经释放了多少底层存储。
