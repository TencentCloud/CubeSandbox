# 模板概览 (Templates)

Template（模板）是 Cube-Sandbox 创建实例的基础镜像和配置快照。本页介绍模板的**概念与生命周期**。

- 使用 CLI 创建、监控、查询或删除模板，请参阅[从 OCI 镜像制作模板](./tutorials/template-from-image.md)。
- 查看模板状态并预览最终请求，请参阅 [模板检查与请求预览](./template-inspection-and-preview.md)。

## 模板生命周期 (三步制作流程)

1. **Init (初始化构建)**
   基于基础镜像（如 Ubuntu）和 Dockerfile，使用 Buildkit 等构建引擎，打包出满足沙箱运行需求的 rootfs 文件系统。

2. **Boot & Snapshot (冷启动与快照)**
   将初始化的 rootfs 放入 MicroVM 中冷启动。等待系统和语言环境（如 Python、Node）完全加载后，对此时的内存和状态打下快照 (Snapshot)。

3. **Deploy (注册与发布)**
   将打包好的 Rootfs 和 Snapshot 文件注册到系统中，成为一个可用的 Template。后续即可通过该 Template 实现沙箱的 **热启动 (Hot Start)**，实现极速启动。

## 将运行中的沙箱提交为模板

`tpl commit` 命令用于将运行中沙箱的当前文件系统和内存状态制作成一个新模板，使用时必须通过 `--sandbox-id` 指定源沙箱。

默认情况下，CLI 仅发送沙箱 ID。CubeMaster 会根据已保存的沙箱规格记录恢复创建该沙箱时使用的完整请求。对于没有规格记录的旧沙箱，CubeMaster 会回退到该沙箱来源模板中保存的创建请求。

CLI 会显示构建进度，并等待模板创建成功或失败。

可以通过 `--file <path>` 提供完整的 `CreateCubeSandboxReq`，覆盖自动恢复的创建请求。该文件会完全替代控制面恢复的请求，两者不会合并。

使用 `--file` 时，可以通过以下网络参数修改文件请求中的 `cube_network_config`：

| 参数                              | 作用                       |
| ------------------------------- | ------------------------ |
| `--allow-internet-access=false` | 显式设置请求中的联网开关。            |
| `--allow-out-cidr <cidr>`       | 追加允许访问的出站 CIDR；该参数可重复传入。 |
| `--deny-out-cidr <cidr>`        | 追加禁止访问的出站 CIDR；该参数可重复传入。 |

网络覆盖参数必须与 `--file` 一同使用。

```bash
# 由 CubeMaster 恢复创建沙箱时使用的请求。
cubemastercli tpl commit \
  --sandbox-id <sandbox-id>

# 使用完整的请求文件覆盖自动恢复的结果。
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json \
  --allow-internet-access=false
```

提交成功后，CubeMaster 会生成一个新的 `tpl-...` 模板 ID，并在源沙箱所在的节点上创建初始副本。

## 下一步

- [从 OCI 镜像制作模板](./tutorials/template-from-image.md) — 完整的 CLI 指南，包括探针配置、进度监控和故障排查。
- [跨机快照](./cross-node-snapshot.md) — `--backend s3`，以及 Resume / FromSnap 落到其他节点的条件。
- [模板检查与请求预览](./template-inspection-and-preview.md) — 如何查看模板状态并预览最终生效的请求。
