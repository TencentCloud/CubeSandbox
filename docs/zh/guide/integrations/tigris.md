---
title: Tigris Volume 集成指南
author: davidmyriel
date: 2026-07-31
tags:
  - integration
  - tigris
  - volume
  - storage
  - s3
lang: zh-CN
---

# Tigris Volume 集成指南

[Tigris](https://www.tigrisdata.com/) 是兼容 S3 协议的对象存储，只对外提供一个全局 Endpoint。本文通过通用的 [S3 Volume 插件](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3)将其接入 CubeSandbox，让沙箱获得可跨节点共享、且在沙箱销毁后依然保留的持久化存储。

插件本身与厂商无关 —— 挂载使用 **s3fs**，控制面使用 **AWS CLI**。本文只覆盖 Tigris 特有的部分：账号、存储桶、访问密钥，以及把插件指向 Tigris 的两个配置项。

## 集成对象与版本

| 项目 | 版本 |
|------|------|
| Cube 平台 | **≥ 0.6.0**（CubeMaster、CubeAPI、Cubelet）—— Volume 框架自 v0.6.0 引入 |
| Python SDK `cubesandbox` | **≥ 0.6.0** |
| S3 Volume 插件 | [`examples/volume/s3`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3) |
| s3fs（`s3fs-fuse`） | ≥ 1.90，支持 `amd64` 与 `arm64` |
| Tigris | S3 API，Endpoint 为 `https://t3.storage.dev` |

## 前置条件

- 一套运行中的 CubeSandbox 部署，包含 CubeMaster、Cubelet、CubeAPI（通常为 `http://<node>:3000`），以及一个沙箱模板 ID。
- 已按照 [S3 插件完整指引](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3)的 §1–§5 安装插件并以 driver 名 `s3` 完成注册 —— 那部分步骤与存储提供方无关，只需执行一次。
- 一个 [Tigris 存储桶](https://www.tigrisdata.com/docs/buckets/create-bucket/)，以及对该桶具备 Editor 权限的[访问密钥对](https://www.tigrisdata.com/docs/iam/create-access-key/)。密钥 ID 以 `tid_` 开头，密钥以 `tsec_` 开头。

::: tip 支持 ARM64
COS 后端无法在 ARM64 上运行 —— 上游 cosfs 仅提供 `amd64` 包，因此 [`docker-install-volume-deps.sh`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/scripts/docker-install-volume-deps.sh) 在 `arm64` 上会直接跳过。S3 插件使用 s3fs，Debian/Ubuntu 与 EPEL 均为 `arm64` 提供了软件包，本集成可在 ARM64 主机上直接运行。
:::

## 集成步骤

### 1. 创建 Tigris 存储桶与访问密钥

在 [Tigris 控制台](https://console.tigris.dev/)中创建存储桶（桶名请勿包含点号 —— s3fs 的 virtual-hosted 寻址方式会导致带点号的桶名 TLS 校验失败），并创建对该桶具备 Editor 权限的访问密钥对。

### 2. 把插件指向 Tigris

在 CubeMaster 与 Cubelet 节点上编辑 `volume-s3.conf`：

```ini
# volume-s3.conf —— root 所有、chmod 600（以明文保存密钥）
ACCESS_KEY_ID=tid_xxx
SECRET_ACCESS_KEY=tsec_xxx
BUCKET=my-cube-volumes
ENDPOINT=https://t3.storage.dev
REGION=auto
```

这就是 Tigris 特有配置的全部内容。Tigris 只提供一个全局 Endpoint 并在内部路由到最近地域，因此不需要选择地域 Endpoint；`REGION=auto` 仅用于 SigV4 签名。

### 3. 在 CubeMaster 上验证连通性

```bash
AWS_ACCESS_KEY_ID=tid_xxx AWS_SECRET_ACCESS_KEY=tsec_xxx AWS_REGION=auto \
  aws s3 ls s3://my-cube-volumes/ --endpoint-url https://t3.storage.dev
```

返回空列表（退出码 0）即为成功。出现 `InvalidAccessKeyId` 或 `AccessDenied` 说明密钥对缺少该桶的 Editor 权限。

### 4. 重启并确认注册成功

```bash
sudo systemctl restart cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -3
```

## 关键代码片段

使用 Python SDK 跑通完整生命周期：

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="s3")

with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from Tigris volume")
    print(sb.files.read("/workspace/hello.txt"))

# 沙箱已销毁，对象仍保留在 Tigris 中
Volume.destroy(vol.volume_id)
```

同一个 Volume 被多个沙箱同时挂载 —— Cubelet 按节点维护引用计数，只有第一次 attach 会真正挂载：

```python
vol = Volume.create("shared-models", driver="s3")

sb1 = Sandbox.create(volume_mounts={"/models": vol})
sb2 = Sandbox.create(volume_mounts={"/models": vol})   # 复用同一个 s3fs 挂载
```

确认对象已写入存储桶：

```bash
aws s3 ls "s3://my-cube-volumes/volumes/" --recursive --endpoint-url https://t3.storage.dev
```

## 注意事项

- **保持默认的 virtual-hosted 寻址方式。** 对 Tigris **不要**设置 `S3FS_EXTRA_OPTS=-ouse_path_request_style`：[2025-02-19 之后创建的存储桶仅支持 virtual-hosted 风格 URL](https://www.tigrisdata.com/blog/virtual-hosted-urls/)，而这正是 s3fs 的默认行为。（这与通常需要 path-style 的 MinIO 恰好相反。）
- **凭证不会进入沙箱。** 凭证保存在 CubeMaster 与 Cubelet 上 root 所有、权限 `600` 的配置文件中，microVM 只看到一个已挂载的文件系统。请勿将其传入模板。
- **对象存储不是 POSIX 文件系统。** s3fs 只是模拟：随机写会重写整个对象，`rename` 实际是「复制再删除」，且跨节点没有文件锁。用于工作区、数据集、模型权重和产物输出没有问题，但不适合数据库文件，也不适合多节点并发写入的构建缓存。
- **RefCount 是按节点统计的。** 同一节点上的两个沙箱共用一个 s3fs 挂载；同一 Volume 在第二个节点上会独立挂载。
- **Destroy 不可恢复**，会删除整个 `volumes/<id>/` 前缀。API 的引用计数保护（有沙箱持有 Volume 时 `DELETE /volumes` 返回 409）是防止删除已挂载 Volume 的机制 —— 真正的风险点是节点崩溃后引用计数失真：集群级计数可能归零而某节点仍持有挂载。
- **官方 e2b Python SDK 无法用于 CubeSandbox** —— 它硬编码指向 e2b.cloud 后端。请使用 `cubesandbox`，或直接调用兼容 e2b 协议的 `/volumes` REST 接口。

## 参考资料

- S3 Volume 插件（本文所配置的通用插件）：[`examples/volume/s3`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume/s3)
- Volume 插件开发框架：[`docs/zh/guide/volume-plugin.md`](../volume-plugin.md)
- 节点本地路径的 Host Mount 方案：[`docs/zh/guide/persistent-storage.md`](../persistent-storage.md)
- Tigris 官网：<https://www.tigrisdata.com/>
- Tigris 文档：<https://www.tigrisdata.com/docs/>
- Tigris S3 兼容性与 AWS CLI 配置：<https://www.tigrisdata.com/docs/sdks/s3/aws-cli/>
- s3fs-fuse：<https://github.com/s3fs-fuse/s3fs-fuse>
