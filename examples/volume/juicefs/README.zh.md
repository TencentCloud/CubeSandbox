# JuiceFS Volume 插件

**插件类型：** `binary` · **驱动名：** `juicefs`

English: [README.md](README.md) · 框架说明：[docs/zh/guide/volume-plugin.md](../../../docs/zh/guide/volume-plugin.md)

把 [JuiceFS](https://github.com/juicedata/juicefs) 文件系统中的一个目录挂进沙箱。JuiceFS 的文件数据存对象存储
（S3、COS、OSS、GCS 等），元数据存 Redis / PostgreSQL / MySQL / TiKV，所以卷用起来就是 POSIX 文件系统：

- 列目录、重命名、追加写、文件锁都很快，git 和 SQLite 可以直接在卷里用；
- 同一节点或不同节点上的沙箱，写入后彼此立刻可见；
- 数据在你自己的对象存储桶里，凭证不进沙箱。

与 [S3 插件](../s3/README.zh.md)（s3fs，每个文件操作一次对象请求）对比，在 AWS 上沙箱内实测
（m7i.xlarge，S3 与 PostgreSQL 元数据库同区域）：

| 操作 | s3 | juicefs | juicefs `--writeback` | 沙箱本地盘 |
|---|---|---|---|---|
| 200 个小文件 | 31.6 s | 6.6 s | 1.2 s | 37 ms |
| 列目录 | 441 ms | 39 ms | 47 ms | 31 ms |
| git init + commit | 12.4 s | 1.4 s | 589 ms | 62 ms |
| SQLite 写入 | 失败 | 301 ms | 118 ms | 61 ms |

卷里只放写一次、读回来的文件，用 S3 插件即可；程序要直接在卷里工作，用本插件。

---

## 工作原理

```
JuiceFS 文件系统（数据：对象存储，元数据：META_URL）
└── volumes/
    ├── vol-aaa/     ← 卷 A：juicefs mount --subdir volumes/vol-aaa
    └── vol-bbb/     ← 卷 B：另一个 juicefs 进程
```

| 阶段 | 触发 | 位置 | 动作 |
|---|---|---|---|
| **create** | `Volume.create()` | CubeMaster | 通过控制面挂载 `mkdir volumes/<id>` |
| **attach** | 创建沙箱 | Cubelet | 节点上第一个沙箱：在 `<volume-base-dir>/juicefs-<id>` 执行 `juicefs mount --subdir volumes/<id>`；之后的沙箱复用 |
| **detach** | 销毁沙箱 | Cubelet | 节点上最后一个沙箱：`juicefs umount --flush` |
| **destroy** | `Volume.destroy()` | CubeMaster | `juicefs rmr volumes/<id>`（开启回收站时先进回收站） |

- **隔离：** 每个卷用 `--subdir` 挂载，沙箱看不到文件系统根目录。
- **控制面挂载：** create / destroy 在 CubeMaster 节点上常驻一个根目录挂载 `/run/cube-volume-juicefs/admin`，
  同时负责 JuiceFS 后台任务（回收站清理、碎片合并）；每个卷的挂载使用 `--no-bgjob`。
- **凭证：** 对象存储凭证由 `juicefs format` 写入元数据，不在插件里。format 时不传 `--access-key` /
  `--secret-key`，即使用节点自身的云身份（如 EC2 实例角色）。插件配置里只有元数据库地址和密码。
- **卷 ID** 必须匹配 `^[A-Za-z0-9._-]{1,128}$`，并拒绝 `.` 与 `..`。这与 CubeAPI、CubeMaster 接受的范围一致，
  它们建出来的卷在这里都能挂上；ID 只作为 `volumes/` 下的一级目录名，不会作为参数传给命令。
- **锁有超时**（`LOCK_WAIT_SECONDS`，默认 60 秒）：上一次调用被杀后残留的锁会让这次 attach 直接报错，
  而不是让后续每一次都无限期阻塞。

---

## 1. 创建文件系统

在任意能访问元数据库的机器上执行一次：

```bash
export META_PASSWORD='<元数据库密码>'
juicefs format 'postgres://juicefs@10.0.0.5:5432/juicefs?sslmode=disable' cube-volumes \
  --storage s3 --bucket https://<bucket>.s3.<region>.amazonaws.com
```

`cube-volumes` 是桶内的对象前缀。元数据库请使用托管或多副本部署并做好备份：它不可用，所有卷都不可用。
引擎选择见 [JuiceFS 文档](https://juicefs.com/docs/zh/community/databases_for_metadata)。

## 2. 安装依赖和插件

在**每个 CubeMaster 和 Cubelet 节点**上：

```bash
sudo ./install-deps.sh --all          # juicefs（固定版本 + 校验和）、fuse、jq
# JUICEFS_VERSION=1.4.2 sudo -E ./install-deps.sh --all   # 需要换版本时显式指定

# 这里装在宿主机上，对应 one-click / systemd 部署。容器化部署（docker、Helm）里，
# CubeMaster 和 Cubelet 在各自容器内执行插件，所以 juicefs 和本脚本要装进镜像；
# 而且与 COS、S3 插件不同，CubeMaster 容器还需要 /dev/fuse 和挂载权限，
# 因为 create/destroy 走的是真实挂载，不是对象存储 API 调用。

DIR=/usr/local/lib/cube-volume-plugins/juicefs
sudo install -d -m 0755 "$DIR"
sudo install -m 0755 cube-volume-juicefs.sh "$DIR/cube-volume-juicefs"
sudo install -m 0600 volume-juicefs.conf.example "$DIR/volume-juicefs.conf"
sudo vi "$DIR/volume-juicefs.conf"    # META_URL、META_PASSWORD、缓存
```

特意放在 `/usr/local/services/cubetoolbox` 之外：升级可能替换该目录并丢失第三方插件
（[#1569](https://github.com/TencentCloud/CubeSandbox/issues/1569)）。任意绝对路径均可。

| 字段 | 说明 | 必填 |
|---|---|---|
| `META_URL` | `juicefs format` 使用的元数据库地址 | 是 |
| `META_PASSWORD` | 元数据库密码，通过环境变量传递，不出现在 `ps` 中 | 否 |
| `CACHE_DIR` | 所有卷共享的本地缓存目录，默认 `/var/jfsCache` | 否 |
| `CACHE_SIZE` | 缓存大小（MiB），默认 `102400` | 否 |
| `MOUNT_OPTS` | 每次挂载附加的 `juicefs mount` 参数 | 否 |

## 3. 注册驱动

**CubeMaster** —— `CubeMaster/conf.yaml`：

```yaml
volume_plugins:
  - name: juicefs
    type: binary
    binary_path: /usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs
```

**Cubelet** —— `Cubelet/config/config.toml`：

```toml
    [[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
      name        = "juicefs"
      type        = "binary"
      binary_path = "/usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs"
```

重启 CubeMaster 和 Cubelet。

## 4. 验证

```bash
P=/usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs
sudo $P --op create  --volume-id smoke --name smoke     # {"token":"","private_data":"volumes/smoke/","error":""}
sudo $P --op attach  --sandbox-id t --namespace default --volume-id smoke --ref-count 0 \
     --volume-base-dir /data/cube-shared/volume          # {"host_path":"/data/cube-shared/volume/juicefs-smoke",...}
echo ok | sudo tee /data/cube-shared/volume/juicefs-smoke/hello.txt
sudo $P --op detach  --sandbox-id t --namespace default --volume-id smoke --ref-count 0 \
     --metadata '{"mount_dir":"/data/cube-shared/volume/juicefs-smoke"}'
sudo $P --op destroy --volume-id smoke
```

使用 SDK：

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-project", driver="juicefs")
with Sandbox.create(template="<template-id>", volume_mounts={"/project": vol}) as sb:
    sb.commands.run("cd /project && git init && echo hi > README && git add . && git commit -m init")
Volume.destroy(vol.volume_id)
```

---

## 性能说明

- **小文件写入。** 默认每个文件在关闭时上传到对象存储，小文件约一次请求一个。`MOUNT_OPTS='--writeback'`
  改为后台上传，上表中 200 个小文件快约 5 倍。数据在上传完成前暂存于 `CACHE_DIR`：节点重启但缓存盘还在，
  重新挂载后继续上传；缓存盘丢失，未上传的数据也会丢失。detach 使用 `--flush` 等待上传完成。
- **海量小文件**（`pip install`、`node_modules`、大仓库 clone）仍远慢于本地盘，建议放沙箱自己的磁盘。
- **内存。** 每个已挂载的卷是节点上的一个 `juicefs` 进程，估算沙箱密度时要算上，或通过 `MOUNT_OPTS` 调小 `--buffer-size`。
- **删除** 先进入 JuiceFS 回收站（format 时的 `--trash-days`，默认 1 天），之后才从桶中删除对象。

## 故障排查

| 现象 | 检查 |
|---|---|
| create 报 `juicefs admin mount failed` | `META_URL` / `META_PASSWORD`；CubeMaster 能否访问元数据库；`/run/cube-volume-juicefs/admin` 与 `/var/log/cube-volume-juicefs/admin.log` |
| attach 报 `juicefs mount failed for volume` | `/dev/fuse`；`/var/log/cube-volume-juicefs/<volume-id>.log`；节点对桶的访问权限（实例角色或 `juicefs format` 时提供的密钥） |
| `unknown driver: juicefs` | CubeMaster 与 Cubelet 以同名注册；升级后插件路径是否还在 |
| 写入慢 | 见[性能说明](#性能说明) |
