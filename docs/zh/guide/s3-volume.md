# S3 持久卷（S3 Volume）

给沙箱一个**跨生命周期持久化**的存储：创建一个 Volume → 挂到沙箱里读写 → 沙箱销毁后数据仍在 → 下次再挂回来继续用。后端是任意 S3 兼容对象存储（AWS S3、Tigris、Cloudflare R2、MinIO……），CubeSandbox 默认安装已自带一个 MinIO 作为后端。

> **默认安装（one-click / Helm 默认值）已经配好了。** 安装脚本会自动起一个 MinIO，并把 S3 Volume 插件、凭证、CubeMaster/Cubelet 配置全部接好。**你不需要手动编辑 `volume-s3.conf`，也不需要装插件** —— 直接跳到 [用法](#用法) 即可。
>
> 只有当你想把后端换成**外部 S3**（AWS S3、Tigris、R2、自建 MinIO 等）时，才需要看 [接入外部 S3](#接入外部-s3)。

---

## 它能做什么

| 能力 | 说明 |
|------|------|
| 跨沙箱持久化 | 沙箱销毁后 Volume 数据保留，下次创建沙箱再挂回来继续读写 |
| 多沙箱共享 | 同一个 Volume 可被多个沙箱同时挂载 |
| 只读挂载 | 每个挂载可单独设只读，适合共享模型/数据集 |
| 任意 S3 兼容后端 | 默认捆绑 MinIO；也可换 AWS S3、Tigris、Cloudflare R2 等 |
| 凭证不进沙箱 | 凭证只在宿主机上，沙箱内只看到一个文件系统 |

---

## 用法

### 1. 安装 SDK

```bash
pip install 'cubesandbox>=0.6.0'
```

::: warning 用 cubesandbox，不要用官方 e2b SDK
官方 e2b Python SDK 会把请求硬编码发到 e2b.cloud 后端，**不能用于 CubeSandbox**。请使用 `cubesandbox`。
:::

### 2. 配置环境变量

在能访问 CubeAPI 的开发机上：

```bash
export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 远程读写已挂载 Volume 时必需（数据面经由 CubeProxy）
export CUBE_PROXY_NODE_IP=<cubeproxy-or-cubelet-node-ip>

# 集群开启鉴权时：
# export CUBE_API_KEY=<your-key>
```

### 3. 创建 Volume 并挂到沙箱

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="s3")   # 默认安装下省略 driver 也走 S3
print("volume_id:", vol.volume_id)

with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from S3 volume")
    print(sb.files.read("/workspace/hello.txt"))   # → from S3 volume

# 沙箱销毁后数据仍在；下次再挂回来，文件还在
with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    print(sb.files.read("/workspace/hello.txt"))

# 真正删除 Volume（后端数据被清空，不可恢复）
Volume.destroy(vol.volume_id)
```

- `volume_mounts` 是 `{沙箱内路径: Volume}` 的映射，一次可挂多个。
- 沙箱销毁**不会**删 Volume 数据；只有 `Volume.destroy()` 才会清空。

### 只读挂载 / 多沙箱共享

```python
from cubesandbox import VolumeMount

vol = Volume.create("shared-dataset", driver="s3")

with Sandbox.create(volume_mounts={"/data": VolumeMount(vol, read_only=True)}) as sb:
    sb.files.read("/data/model.bin")
```

---

## 接入外部 S3

默认安装用内置 MinIO。想换成外部 S3（AWS S3、Tigris、R2、自建 MinIO 等），按你的部署方式改对应的"源"——安装器会自动生成 `volume-s3.conf`，**不要手动编辑该文件**（它会被重写）。

### one-click 部署

在 `.one-click.env`（或 `env.example`）里关掉内置 MinIO、填 `CUBE_S3_*`，重跑 `install.sh`，它会写出 `volume-s3.conf`：

```bash
CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_S3_ENDPOINT=https://s3.example.com
CUBE_S3_ACCESS_KEY_ID=...
CUBE_S3_SECRET_ACCESS_KEY=...
CUBE_S3_BUCKET=cube-volumes
# CUBE_S3_REGION=us-east-1
# CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
```

计算节点不部署 MinIO，把控制节点 `.one-click.env` 里的 `CUBE_S3_*` 拷过去即可。详见 [one-click README · 内置 MinIO 与 S3 Volume 插件](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README_zh.md)。

### Helm 部署

在 values 里关掉 chart MinIO、填 `volumeS3.*`（或用 `volumeS3.existingSecret` 指向已有 Secret），`helm upgrade` 后 chart 会渲染 `volume-s3.conf` Secret：

```yaml
minio:
  enabled: false
volumeS3:
  endpoint: https://s3.example.com
  accessKeyId: ...
  secretAccessKey: ...
  bucket: cube-volumes
  # region: us-east-1
  # extraOpts: -ouse_path_request_style
```

详见 chart `values.yaml` 的 `volumeS3` 段。

### 从零手动部署（非 one-click / 非 Helm）

手动部署插件、手写 `volume-s3.conf` 的完整步骤见 [`examples/volume/s3/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.zh.md)；厂商级端到端（开通账号、建桶、取密钥）见 [Tigris 集成指南](./integrations/tigris.md)。

### 常见后端

| 提供方 | `ENDPOINT` | `REGION` |
|--------|-----------|----------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | 桶所在地域 |
| Tigris | `https://t3.storage.dev` | `auto` |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| MinIO | `http://<minio-host>:9000` | 任意值 |

---

## 常见问题

### S3 Volume 和 Host Mount 有什么区别？

[Host Mount](./persistent-storage.md) 把**节点上已有的目录**绑定进沙箱，节点本地、适合共享现成数据。S3 Volume 是**用户级持久卷**（e2b Volume API），由插件按需创建/销毁、跨沙箱生命周期管理、后端是对象存储。两者互补：Host Mount 适合"节点上已有目录"，S3 Volume 适合"每个用户/任务一个独立持久卷"。

### 省略 `driver` 会怎样？为什么默认走 S3？

`Volume.create(...)` 省略 `driver` 时，CubeMaster 取 `volume_plugins` 列表的**第一项**。默认安装现已把 `s3` 排在第一，所以省略 `driver` 即走 S3。显式写 `driver="s3"` 仍推荐——在多后端集群里意图更清晰，也避免有人把 `cos` 调到前面后行为悄悄变化。

### 沙箱销毁后数据还在吗？Volume 的生命周期是怎样的？

在。Volume 的挂载/卸载由**引用计数**驱动，你不需要手动管挂载点：

| 操作 | 行为 |
|------|------|
| `Volume.create` | 在后端建一个 `volumes/<id>/` 前缀 |
| 沙箱挂载（refCount 0→1） | Cubelet 用 s3fs 把该前缀挂到宿主机，再透传进 microVM |
| 沙箱挂载（refCount >0） | 复用已有挂载，不重复 mount |
| 沙箱销毁（refCount >1） | 仅减计数，不卸载 |
| 沙箱销毁（refCount →0） | `fusermount -u` 卸载，**后端数据保留** |
| `Volume.destroy` | 删除整个 `volumes/<id>/` 前缀，不可恢复 |

### 删除 Volume 会怎样？为什么有时返回 409？

`Volume.destroy` 会清空后端整个 `volumes/<id>/` 前缀，**不可恢复**。有沙箱仍持有该 Volume 时 API 会拒绝并返回 409——先销毁所有使用它的沙箱再删。注意节点崩溃后引用计数可能失真（集群级计数归零但某节点仍持有挂载），删除前请确认。

### 桶必须提前创建吗？

不必。Create 会先 `head-bucket`，桶不存在时自动创建；桶已存在则直接用，不需要 `s3:CreateBucket` 权限。

---

## 故障排查

| 现象 | 排查方向 |
|------|----------|
| `unknown driver: s3` | CubeMaster `volume_plugins` 没注册 `s3`，或没重启 |
| `no plugin registered for driver "s3"` | Cubelet 没注册同名插件，或没重启 |
| attach 失败 `s3fs mount failed` | 宿主机 `ls /dev/fuse` 是否存在；凭证/`ENDPOINT` 是否正确 |
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | 密钥错、缺桶权限，或 `REGION` 与 Endpoint 期望的 SigV4 地域不符 |
| 桶名含点号 | s3fs 默认 virtual-hosted 寻址，带点的桶名会 TLS 失败。改用无点桶名，或在 conf 里设 `S3FS_EXTRA_OPTS=-ouse_path_request_style`（MinIO 通常也需要） |
| SDK 写入失败 | 没设 `CUBE_PROXY_NODE_IP`；或 CubeAPI/模板未就绪 |
| `Volume.create` 没走 s3 | 省略 `driver` 时取 `volume_plugins` **第一项**；默认安装下第一项是 `s3`。若你的集群把 `cos` 调到了前面，请显式写 `driver="s3"` |

更多排障与协议细节：[Volume 插件开发指南](./volume-plugin.md)、[S3 插件完整指引](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.zh.md)。
