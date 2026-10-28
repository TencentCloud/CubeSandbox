# JuiceFS 服务配置

[JuiceFS](https://juicefs.com/docs/zh/community/introduction/) 是一个开源、POSIX 兼容的分布式文件系统。JuiceFS 采用数据与元数据分离的设计，数据存储在对象存储，元数据存储在元数据存储，支持接入使用 Redis、MySQL、TiKV 等元数据存储和 MinIO、S3、COS 等对象存储。

Cube 的 JuiceFS 存储方案架构见 [JuiceFS 跨机存储架构](/zh/architecture/juicefs-storage)，性能测试数据见 [JuiceFS 性能测试](/zh/blog/posts/2026-10-28-cubesandbox-juicefs-perf-benchmark)。

关于 JuiceFS 的更多内部设计见 JuiceFS 官方的[技术架构](https://juicefs.com/docs/zh/community/architecture/)。

## 前置条件

- 所有节点需要 FUSE 支持
- 所有节点能够访问同一个 JuiceFS 元数据引擎和对象存储。

## 安装部署

默认 one-click 不会启用 JuiceFS 存储方案，需要手动配置启用。

可以在安装 one-click 时编辑 `.env` 文件配置相关信息，也可以在安装后编辑 cube 全局配置文件 `.one-click.env`。

```bash
# one-click 包的 .env 文件路径
cp env.example .env

# 或者安装完成后编辑配置文件 /usr/local/services/cubetoolbox/.one-click.env
vim /usr/local/services/cubetoolbox/.one-click.env
```

### 控制节点初始化

首先在应在控制节点配置信息并初始化 JuiceFS 文件系统，然后再在各计算节点进行配置。

Cube 默认使用 Redis 作为 JuiceFS 元数据引擎，使用 MinIO 作为对象存储。

> 后续如需切换其他元数据引擎或对象存储，见下文[更换存储](#更换存储)。

设置 `ONE_CLICK_ENABLE_JFS=1`：

```bash
ONE_CLICK_ENABLE_JFS=1
```

初始化 JuiceFS 文件系统，在控制节点执行如下命令：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/init.sh
```

然后挂载控制节点 JuiceFS 文件系统：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

### 计算节点初始化

编辑计算节点的配置文件,设置 `ONE_CLICK_ENABLE_JFS=1`。

```bash
ONE_CLICK_ENABLE_JFS=1
```

执行如下命令在计算节点挂载 JuiceFS 文件系统：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

`ONE_CLICK_ENABLE_JFS` 及上述路径配置会保存到 `.one-click.env`。后续升级即使替换了
Cubelet 的默认 `config.toml`，Cubelet 启动脚本也会在进程启动前自动恢复 `[cow.jfs]`
的 `enable`、`mount_path`、`root` 和 `cache_dir`，无需再次手工修改配置。

### 在控制节点检查

所有节点挂载完成后，在控制节点执行脚本检查 JuiceFS 文件系统挂载状态：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh
```

输出类似如下内容：

```bash
JuiceFS volume
  Name:          cube
  Storage:       s3
  Bucket:        http://<control-node-ip>:9000/cube-jfs
  Active clients: 3
  Used space:    12288 bytes

Active client sessions
  SID  CLIENT HOST   REPORTED ADDRESSES  MOUNT PATH         MOUNT TIME
  ---  ------------  ------------------  -----------------  ------------------------------
  2    a26a3780ac6f  172.17.0.2          /data/cubelet/jfs  2026-10-28T08:46:15.482143729Z
  1    c9af1d5570f4  172.17.0.3          /data/cubelet/jfs  2026-10-28T08:43:29.693002719Z
  3    30e38b8072fe  172.17.0.2          /data/cubelet/jfs  2026-10-28T08:49:31.946778689Z
```

## 节点变更

### 新增节点

1. 按正常流程安装并注册新的 Cube 计算节点；
2. 在新节点的 `.one-click.env` 写入 `ONE_CLICK_ENABLE_JFS=1`；
3. 在新节点执行 `sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh`；
4. 回到控制节点执行 `sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh`，确认新会话出现。

控制节点只需要在初始化的时候执行一次 `/usr/local/services/cubetoolbox/scripts/cube-jfs/init.sh`，**后续新增节点不需要再次执行**。

### 删除节点

先按[节点相关操作](/zh/guide/node-operations)隔离节点并清空沙箱。然后在待删除节点执行：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh unmount
```

再在控制节点删除节点记录：

```bash
cubeopscli node delete <node-id>
```

卸载节点不会删除共享 JuiceFS 中的数据。不要直接删除 `CUBE_JFS_ROOT/nodes/<node-name>`；其中可能仍有模板、快照或暂停数据被集群引用。

## 更换存储

### 从 MinIO 迁移到其他对象存储

修改配置文件使用其他对象存储：

```bash
ONE_CLICK_ENABLE_JFS=1

# 可继续使用现有 Redis；新卷时必须是新的、空的元数据地址。
CUBE_JFS_META_URL="redis://<redis-host>:6379/0"
CUBE_JFS_META_PASSWORD="<redis-password>"

# 例如使用腾讯云 COS
CUBE_JFS_STORAGE="cos"
CUBE_JFS_BUCKET="https://<bucket>-<appid>.cos.<region>.myqcloud.com"
CUBE_JFS_ACCESS_KEY="<COS-SecretId>"
CUBE_JFS_SECRET_KEY="<COS-SecretKey>"
```

然后参考[安装部署](#安装部署)初始化 JuiceFS 文件系统进行控制节点和计算节点初始化。

请注意，目前 Cube 暂不支持从对象存储 A 平滑切换到对象存储 B，请在 one-click 安装时指定需要使用的对象存储，或者参考 JuiceFS 官方的[数据同步](https://juicefs.com/docs/zh/community/guide/sync/)文档进行数据迁移。

### 使用 Redis 之外的元数据引擎

`CUBE_JFS_META_URL` 可以使用 JuiceFS 支持的 MySQL、PostgreSQL、TiKV 等地址。例如：

```bash
CUBE_JFS_META_URL="mysql://user:@(10.0.0.20:3306)/juicefs"
CUBE_JFS_META_PASSWORD="password"
```

新集群直接填写该地址后执行控制节点初始化即可。已有卷迁移时，先停止写入并卸载所有客户端，再用 `juicefs dump` 从旧元数据引擎导出、用 `juicefs load` 导入一个空的新元数据引擎，随后更新所有节点的 `CUBE_JFS_META_URL` 并重新挂载。对象存储密钥默认不会包含在 dump 中，需要在新元数据引擎上重新配置。参考 JuiceFS 官方的[元数据备份与迁移](https://juicefs.com/docs/zh/community/metadata_dump_load/)文档。

## 镜像分发

目标节点默认从镜像仓库拉取 `juicedata/mount:ce-v1.4.1`。

如果是离线环境，可以提前 `docker load` 下载镜像，并通过修改配置文件中的 `CUBE_JFS_IMAGE` 指向本地镜像名。

升级镜像时应让所有节点使用同一个固定版本。

## 排障

### 服务无法启动

先查看 systemd 和容器日志：

```bash
journalctl -u cube-sandbox-jfs.service -n 200 --no-pager
docker logs cube-sandbox-jfs
docker logs cube-sandbox-jfs-redis
```

常见原因包括：

- `CUBE_JFS_META_URL` 或 `CUBE_JFS_META_PASSWORD` 未正确写入 `.one-click.env`，或者各节点使用的值不一致；
- 节点没有 `/dev/fuse`，或 Docker 服务未启动；
- 无法拉取 `CUBE_JFS_IMAGE`；
- Redis、MinIO/S3 地址不可达，或 format 时使用的凭证失效。

### 容器运行但宿主机看不到挂载

```bash
findmnt -T /data/cubelet/jfs
docker inspect cube-sandbox-jfs --format '{{.State.Status}}'
```

挂载脚本会为挂载点建立 self bind，并设置 `rshared` 传播。不要绕过 systemd 单元直接运行一个没有 `bind-propagation=rshared` 的容器，否则 FUSE 挂载可能只在容器命名空间内可见。

### Cubelet 报 `jfs storage is not configured`

确认 `[cow.jfs] enable = true`，并检查 `mount_path`、`root` 和 `cache_dir` 是否与 `.one-click.env` 一致。修改后重启 Cubelet：

```bash
sudo systemctl restart cube-sandbox-cubelet.service
```

### `remote_status` 长时间停在 `inprogress`

使用 `--writeback` 时，文件关闭后仍可能有 block 留在本地 staging 区。Cubelet 会等相关 block 上传完成后才登记为 `ready`。检查 JuiceFS 容器日志、对象存储连通性、缓存盘空间和上传带宽；不要直接删除 cache 目录中的 staging 数据。

### 只能在源节点恢复

依次确认：资源的 `backend` 是 `jfs`、`remote_status` 已为 `ready`、所有节点使用相同的元数据地址与 `root`，并且目标节点的 CPU/kernel 满足[跨机恢复条件](./requirements.md)。host-mount 仍会把沙箱固定在源节点。
