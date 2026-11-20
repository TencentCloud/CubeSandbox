# NFS 卷插件

[English](README.md)

`nfs` 二进制 Volume 插件让不同 Worker 上的沙箱共享一个 NFSv3 目录。Cubelet 按需挂载卷，通过 virtiofs 将目录提供给沙箱。同一 Worker、同一 Cubelet namespace 内的沙箱共享底层挂载，最后一个沙箱解除绑定后才卸载。

## 构建与配置

在本目录中为目标 Linux 架构构建二进制：

```bash
go build -o cube-volume-nfs ./cmd/cube-volume-nfs
```

将二进制安装到 CubeMaster 和所有可能承载该卷的 Cubelet。一键部署和 Kubernetes 发布包尚未包含此插件，需要单独构建、安装。每个 Cubelet 都需要 Linux NFS 客户端工具、挂载文件系统的权限，以及到 NFS 服务器和 `mountd` 的网络连接。

在 CubeMaster 的 `conf.yaml` 中注册插件：

```yaml
volume_plugins:
  - name: nfs
    type: binary
    binary_path: /opt/cube-plugins/cube-volume-nfs
```

CubeMaster 使用列表中的第一个卷插件作为默认驱动。除非所有默认调用都能提供明确的、预先准备好的 NFS 卷名，否则应将现有默认驱动保留在首位。本文示例通过 `driver="nfs"` 显式选择插件。

在 Cubelet 的 `config.toml` 中注册插件：

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name = "nfs"
  type = "binary"
  binary_path = "/opt/cube-plugins/cube-volume-nfs"
```

在每个 CubeMaster 上挂载同一个 NFS export，并将 `CUBE_NFS_CONTROL_ROOT` 设置为该挂载的绝对路径，例如 `/mnt/nfs-volumes`。CubeMaster 需要在预建卷目录内创建绑定文件和硬链接的权限。

在 Cubelet 进程环境中设置 `CUBE_NFS_EXPORT`，例如 `nfs.example:/exports/volumes`。插件按需挂载 `<export>/<volume-id>`，验证 CubeMaster 保存的绑定，只向沙箱暴露其 `data` 子目录。export 必须允许 Worker 以读写方式挂载这些子目录。Cubelet 进程的 `PATH` 中应包含 `mount.nfs` 和 `umount`。部署和 SDK 配置参见 [Volume 插件指南](../../../docs/zh/guide/volume-plugin.md)。

## 注册与挂载

必须显式指定卷名，并与预建目录名一致。卷名由 1 到 128 个 ASCII 字母、数字、下划线或连字符组成。省略名称时 CubeMaster 会生成 UUID，但对应目录尚未预建，注册将失败。注册前请在 NFS 服务器上创建 `data` 目录，并设置所需文件系统权限。以下示例使用 `/exports/volumes/session-1/data`，注册名为 `session-1`。

此插件假定一个 export 内的调用者属于同一信任域。首次成功的 Create 会认领尚未绑定的目录；绑定能阻止后续注册复用目录，但不验证首次调用者是否有权认领。插件不实施租户所有权校验，共享 export 中的卷应由可信操作人员注册。若调用者互不信任，必须在 Volume API 之前由外部授权层限制目录注册和挂载权限。

```python
from cubesandbox import Sandbox, Volume, VolumeMount

volume = Volume.create("session-1", driver="nfs")

with Sandbox.create(volume_mounts={"/workspace": volume}) as writer:
    writer.files.write("/workspace/example.txt", "shared data")
    with Sandbox.create(
        volume_mounts={"/workspace": VolumeMount(volume, read_only=True)},
    ) as reader:
        print(reader.files.read("/workspace/example.txt"))
```

底层挂载使用 `rw,nosuid,nodev`。复用挂载前，插件会检查其来源和访问模式。Cubelet 通过 bind mount 和 virtiofs 为每个沙箱实施只读限制。实际写权限取决于 NFS export、UID/GID 映射、目录权限、ACL 和 `root_squash`。外部应用也可使用同一 NFS 目录。父目录应由平台管理，并限制沙箱工作负载直接访问 export 的网络权限。

使用测试卷验证跨 Worker 读写、只读沙箱写入被拒绝，以及沙箱退出后数据仍然存在。

目录未预建、卷 ID 无效、目录已被认领等调用错误返回 `error_kind: "invalid_argument"`，经 CubeMaster 映射为 HTTP 400。控制根目录配置错误或文件系统 I/O 失败仍属于内部错误。公开的 Hook 错误不包含服务器路径或挂载工具输出；诊断详情写入 stderr。二进制驱动可能在进程成功退出后丢弃 stderr，排障时可能需要显式捕获。

插件采用前台 hard NFS 挂载。[nfs(5)](https://man7.org/linux/man-pages/man5/nfs.5.html) 说明，`retry=0` 对前台和后台挂载都表示首次失败后停止重试；它不限制单次尝试的耗时，也不限制后续文件系统 I/O。每个 Hook 使用 20 秒上下文，超时后取消外部命令。这不是所有文件系统操作的严格时限：服务器故障时，应用 I/O、宿主机路径检查或处于不可中断内核 I/O 的辅助进程仍可能等待服务器恢复。Attach 失败后的清理另用 3 秒上下文。请据此安排服务器可用性和故障恢复。

## 注销并保留数据

所有沙箱解除绑定后，调用 `Volume.destroy(volume.volume_id)` 注销卷。NFS 目录及其绑定会保留；只要原目录仍在，重新注册同一个 ID 就会被拒绝。

需要复用 ID 时，存储管理员应将旧目录退役，并重新准备 `<volume-id>/data`。迁移保留数据时应将绑定与整个旧目录一起移动。

### 恢复失败的注册

Create 可能先发布绑定，CubeMaster 随后才提交数据库记录。如果数据库更新失败，回滚会调用 Destroy；该操作有意保留数据和绑定，因此再次注册会提示目录已被认领。

恢复前，停止该 ID 的并发注册请求，并确认 CubeMaster 中没有对应卷记录，所有 Worker 上也没有挂载引用。存储管理员随后可将包含绑定的旧目录退役，准备新的 `<volume-id>/data`，迁移所需数据，再以相同的显式名称注册。不要删除使用中卷的绑定。

## 验证

在本目录运行 `go test ./...`，覆盖绑定、挂载元数据、引用计数、卸载和错误分类。测试使用临时目录及注入的挂载操作，不会挂载真实 NFS 服务器。

共享的 `tests/e2e/sdk_compat/cases/volume` CRUD 用例会随机生成卷名，不能仅设置 `SDK_E2E_VOLUME_DRIVER=nfs` 就原样运行。应先预建独立的 `<volume-id>/data`，再参考上述 SDK 示例，在测试 fixture 中使用该显式 ID。验证一个沙箱写入、另一 Worker 上的沙箱读取、`VolumeMount(..., read_only=True)` 拒绝写入，以及两者解除绑定后的数据保留。最后注销测试卷，保留或退役包含绑定的目录；再次测试注册前需准备新的目录。
