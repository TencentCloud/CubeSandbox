# JuiceFS 服务配置

[JuiceFS](https://juicefs.com/docs/zh/community/introduction/) 是一个开源、POSIX 兼容的分布式文件系统。JuiceFS 采用数据与元数据分离的设计，数据存储在对象存储，元数据存储在元数据存储，支持接入使用 Redis、MySQL、TiKV 等元数据存储和 MinIO、S3、COS 等对象存储。

CubeSandbox 借助 JuiceFS 的能力实现跨机存储方案，具体架构见 [JuiceFS 跨机存储架构](/zh/architecture/juicefs-storage)，性能测试数据见 [JuiceFS 性能测试](/zh/blog/posts/2026-11-04-cubesandbox-juicefs-perf-benchmark)。

::: warning 预览版
JuiceFS 存储方案目前处于 Preview 预览阶段，适合快速上手与早期试用。配置项、部署脚本和运行行为在后续版本中仍可能调整。默认搭配控制节点 Redis/MinIO 仅适用于试用；生产环境请先在测试环境验证，并参考[更换存储](#更换存储)切换到更可靠的元数据引擎与对象存储。
:::

## 前置条件

- 所有节点的内核需要 FUSE 支持
- 所有节点能够访问同一个 JuiceFS 元数据引擎和对象存储。

## one-click 安装部署

默认 one-click 不会启用 JuiceFS 跨机存储方案，需要手动配置启用。

可以在安装 one-click 时编辑 `.env` 文件配置相关信息，也可以在安装后编辑 cube 全局配置文件 `.one-click.env`。

```bash
# one-click 包的 .env 文件路径
cp env.example .env

# 或者安装完成后编辑配置文件 /usr/local/services/cubetoolbox/.one-click.env
vim /usr/local/services/cubetoolbox/.one-click.env
```

### 控制节点初始化

首先在应在控制节点配置信息并初始化 JuiceFS 文件系统，然后再在各计算节点进行配置。

Cube 默认使用控制节点上的 Redis 作为 JuiceFS 元数据引擎，使用控制节点上的 MinIO 作为对象存储。

> **该方案仅适用于快速上手尝试，后续如需在生产环境部署，请切换到其他元数据引擎或对象存储**，见下文[更换存储](#更换存储)。

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

> 执行了 mount.sh 脚本后 JuiceFS 挂载将会开机自启。

### 在控制节点检查

所有节点挂载完成后，在控制节点执行脚本检查 JuiceFS 文件系统挂载状态：

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh
```

应当输出类似如下内容：

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


### 更换存储

#### 从 MinIO 迁移到其他对象存储

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

然后参考[one-click 安装部署](#one-click-安装部署)进行控制节点和计算节点初始化。

请注意，目前 Cube 暂不支持从对象存储 A 平滑切换到对象存储 B，请在 one-click 安装时指定需要使用的对象存储，或者参考 JuiceFS 官方的[数据同步](https://juicefs.com/docs/zh/community/guide/sync/)文档进行数据迁移。

#### 使用 Redis 之外的元数据引擎

> JuiceFS 的元数据存储非常重要，如果在生产环境部署，推荐使用 Redis 集群以降低数据丢失的风险。

`CUBE_JFS_META_URL` 可以使用 JuiceFS 支持的 MySQL、PostgreSQL、TiKV 等地址。例如：

```bash
CUBE_JFS_META_URL="mysql://user:@(10.0.0.20:3306)/juicefs"
CUBE_JFS_META_PASSWORD="password"
```

新集群直接填写该地址后执行控制节点初始化即可。已有卷迁移时，先停止写入并卸载所有客户端，再用 `juicefs dump` 从旧元数据引擎导出、用 `juicefs load` 导入一个空的新元数据引擎，随后更新所有节点的 `CUBE_JFS_META_URL` 并重新挂载。对象存储密钥默认不会包含在 dump 中，需要在新元数据引擎上重新配置。参考 JuiceFS 官方的[元数据备份与迁移](https://juicefs.com/docs/zh/community/metadata_dump_load/)文档。

## Kubernetes Helm 部署

请先参考本文档中的 [k8s 部署的 Helm 安装](/zh/guide/kubernetes/install) 章节，安装 Helm 并完成 Kubernetes 集群的初始化。

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

默认配置不会启用 JuiceFS 跨机存储方案，需要手动修改 `runtime-values.yaml` 文件：

```yaml
cubeJuiceFS:
  enabled: true
```

启用后 Chart 会额外部署：专供 JuiceFS 使用的独立 Redis、一个只在首次执行格式化的 Job，以及在每个计算节点挂载文件系统的 DaemonSet。

参考 Helm 部署章节中的 [Helm 安装](/zh/guide/kubernetes/install#_5-helm-安装) 章节，使用对应的多节点/腾讯云TKE/单节点集群的 Helm 安装命令进行安装。

```bash
# 多节点集群安装命令
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```

如果 Cube 已经安装，但之前没启用 JuiceFS；现在升级并首次启用 JuiceFS 跨机存储方案，则启用过程会重建 Cube Node Pod。应先停止或迁移所有计算节点上正在运行的沙箱，再临时配置 `lifecycleChangeAck: true`：

```yaml
cubeJuiceFS:
  enabled: true
  lifecycleChangeAck: true
```

第一次 `helm upgrade` 成功后，把 `lifecycleChangeAck` 改回 `false`，再执行
一次相同的 `helm upgrade`。

> 第二次升级只清除 Helm release 中的一次性确认，不会重启 Cube Node、JuiceFS 或 Redis。之后可以保留 `false`，也可以从
`runtime-values.yaml` 删除该行。`lifecycleChangeAck` 不是 JuiceFS 日常配置。

以后修改 JuiceFS 镜像、存储地址或挂载参数时的维护步骤，见
[Kubernetes 升级说明](/zh/guide/kubernetes/upgrade#修改-juicefs-开关或存储配置)。


### 验证 Kubernetes 部署

```bash
kubectl get job,daemonset,pod -n cube-system

kubectl exec -n cube-system daemonset/cube-juicefs -c juicefs -- \
  findmnt -T /data/cubelet/jfs

kubectl exec -n cube-system daemonset/cube-node -c cubelet -- \
  findmnt -T /data/cubelet/jfs
```

两次检查都必须显示 `fuse.juicefs`。之后创建模板时指定 `--backend jfs`；
已有 XFS 模板不会自动转换为 JFS。


### 更换存储

Chart 默认创建独立的 JuiceFS Redis，并使用 Chart MinIO 中的 `cube-jfs` bucket。本节仅用于替换这两个默认后端，
外部 Redis 和外部对象存储可以单独替换，不要求同时配置。


#### 从 MinIO 迁移到其他对象存储

先创建对象存储 Secret 以避免明文存储密钥：

```bash
kubectl -n cube-system create secret generic cube-jfs-object \
  --from-literal=access-key='<access-key>' \
  --from-literal=secret-key='<secret-key>'
```

然后在 `runtime-values.yaml` 中增加 `storage`：

```yaml
cubeJuiceFS:
  enabled: true
  storage:
    type: s3
    bucket: https://<bucket>-<appid>.cos.<region>.myqcloud.com/cube-jfs
    existingSecret: cube-jfs-object
```

`storage.bucket` 是传给 JuiceFS 的完整 `--bucket` 值，不只是桶名；路径风格的 S3 兼容存储要把桶名写进 URL，例如 `http://minio.example.com:9000/cube-jfs`。


请注意，目前 Cube 暂不支持从对象存储 A 平滑切换到对象存储 B，请在安装时指定需要使用的对象存储，或者参考 JuiceFS 官方的[数据同步](https://juicefs.com/docs/zh/community/guide/sync/)文档进行数据迁移。


#### 使用 Redis 之外的元数据引擎

> JuiceFS 的元数据存储非常重要，如果在生产环境部署，推荐使用 Redis 集群以降低数据丢失的风险。

只替换 Redis 时，先创建密码 Secret：

```bash
kubectl -n cube-system create secret generic cube-jfs-meta \
  --from-literal=meta-password='<redis-password>'
```

然后在 `runtime-values.yaml` 中增加 `metadata`：

```yaml
cubeJuiceFS:
  enabled: true
  metadata:
    url: redis://redis.example.com:6379/0
    existingSecret: cube-jfs-meta
    secretRevision: initial
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

## JFS 相关配置说明

`CUBE_JFS_*` 系列变量可以在 `.env` / `.one-click.env` 中调整，大部分配置项不建议修改，这里说明几个需要关注的配置项。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `CUBE_JFS_MOUNT_PATH` | `/data/cubelet/jfs` | 挂载点|
| `CUBE_JFS_CACHE_DIR` | `/data/cubelet/jfs-cache` | 本地缓存目录，读缓存与写 staging 都放在其中，建议放在节点的 NVMe/SSD 数据盘上 |
| `CUBE_JFS_CACHE_SIZE_MB` | `102400`（100 GiB） | 读缓存大小上限 |

### 缓存目录与容量建议

`CUBE_JFS_CACHE_DIR` 中同时存放两类数据，目录结构见 [JuiceFS 跨机存储架构](/zh/architecture/juicefs-storage)：

- 读缓存（`raw/`）：从对象存储拉回的数据块，可随时按 LRU 淘汰，受 `CUBE_JFS_CACHE_SIZE_MB` 上限约束；
- 写 staging（`rawstaging/`）：`--writeback` 下尚未上传到对象存储的脏数据块，上传完成后即删除，不占用 `CUBE_JFS_CACHE_SIZE_MB` 配额。

`CUBE_JFS_CACHE_SIZE_MB` 默认 100 GiB，与[性能测试](/zh/blog/posts/2026-11-04-cubesandbox-juicefs-perf-benchmark)所用配置一致。注意它是一个**上限而不是预留空间**，缓存按需增长：

- 磁盘可用空间不足 100 GiB 时挂载不会失败，读缓存只会增长到实际可用的大小；
- JuiceFS 默认 `--free-space-ratio 0.1`：当缓存所在文件系统剩余空间低于 10% 时，客户端按近似 LRU 的 2-random 算法主动淘汰读缓存块，直到剩余空间回到阈值以上。被淘汰的块下次读取时需重新从对象存储拉取，缓存命中率下降；
  
  **请注意，该参数的 10% 是相对于该目录所在的数据盘总容量计算的**，即 `/data/cubelet/jfs-cache` 缓存目录所在数据盘 `/data` 总容量为 1000 GiB，那么即使 `/data/cubelet/jfs-cache` 已使用 100 GiB，剩余空间也高于 10%，该兜底永远不会触发，读缓存的实际限制就是 `CUBE_JFS_CACHE_SIZE_MB`。
- 若磁盘被缓存之外的数据（Cubelet 数据、镜像、staging 等）持续占满，淘汰只能暂时腾出空间：客户端会反复淘汰以守住阈值，读缓存被持续压缩，越来越多读请求退化为直读对象存储，性能明显下降；
- 剩余空间低于阈值的一半（默认 5%）时，staging 写入同样受限：JuiceFS 会提前上传 staging 中的块以释放空间，新写入可能退化为同步写对象存储。必要时可通过 `CUBE_JFS_MOUNT_OPTS` 调整 `--free-space-ratio`（例如 0.2）。
- staging 没有独立配额：若数据盘被写满，`--writeback` 下的新写入会直接失败，快照、暂停等操作可能报错，因此必须为 staging 和其他数据预留空间。

建议将缓存目录放在独立的快速数据盘上，`CUBE_JFS_CACHE_SIZE_MB` 不超过该盘可用空间的 70%–80%；数据盘较小（如 100–200 GiB）时按比例调小该值。读缓存的淘汰通常先由 `CUBE_JFS_CACHE_SIZE_MB` 触发；`--free-space-ratio` 更多是在缓存盘被其他数据（尤其是 staging 积压）持续填满时兜底，缓存盘越宽裕该兜底触发越晚，建议同时监控缓存盘整体使用率。

## 镜像分发

目标节点默认从镜像仓库拉取 `juicedata/mount:ce-v1.4.1`。

如果是离线环境，可以提前 `docker load` 下载镜像，并通过修改配置文件中的 `CUBE_JFS_IMAGE` 指向本地镜像名。Kubernetes 部署中改为在 values 里设置 `images.juicefs.repository` / `tag`。

非必要无需升级 JuiceFS 镜像，应让所有节点使用同一个固定版本。

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

### Kubernetes：首次安装时格式化 Job 失败

元数据或对象存储凭证配置错误时，格式化 Job 会重试耗尽后失败，挂载 Pod 一直停在 `Init:CrashLoopBackOff`。修正凭证后，删除失败的 Job 再执行相同的 `helm upgrade`——Job 名称只在 spec 变化时才会变，所以它不会自行重试：

```bash
kubectl -n cube-system delete job -l app.kubernetes.io/component=cube-juicefs-format
```

### Kubernetes：不要删除 Chart 创建的 JuiceFS 元数据 Secret

元数据 Secret 保存的是 Chart 管理的 JuiceFS Redis 密码，Helm 升级时会通过 lookup 保留现有密码。删除该 Secret 后 Chart 会生成新的随机密码，而运行中的 Redis 仍在使用旧密码，之后新建的 JuiceFS Pod 全部认证失败。误删后，把 `cubeJuiceFS.metadata.password` 设为 Redis 实际使用的密码（可从 Redis Pod 内的 `/etc/redis/redis-auth.conf` 读取），临时设置 `cubeJuiceFS.lifecycleChangeAck: true` 重新升级，完成后移除确认键。
