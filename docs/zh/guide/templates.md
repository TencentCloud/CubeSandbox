# 模板概览 (Templates)

Template（模板）是 Cube-Sandbox 创建实例的基础镜像和配置快照。本页介绍模板的**概念与生命周期**。

- 使用 CLI 创建、监控、查询或删除模板，请参阅[从 OCI 镜像制作模板](./tutorials/template-from-image)。
- 查看模板状态并预览最终请求，请参阅 [模板检查与请求预览](./template-inspection-and-preview)。

## 模板生命周期 (三步制作流程)

1. **Init (初始化构建)**
   基于基础镜像（如 Ubuntu）和 Dockerfile，使用 Buildkit 等构建引擎，打包出满足沙箱运行需求的 rootfs 文件系统。

2. **Boot & Snapshot (冷启动与快照)**
   将初始化的 rootfs 放入 MicroVM 中冷启动。等待系统和语言环境（如 Python、Node）完全加载后，对此时的内存和状态打下快照 (Snapshot)。

3. **Deploy (注册与发布)**
   将打包好的 Rootfs 和 Snapshot 文件注册到系统中，成为一个可用的 Template。后续即可通过该 Template 实现沙箱的 **热启动 (Hot Start)**，实现极速启动。

## 下一步

- [从 OCI 镜像制作模板](./tutorials/template-from-image) — 完整的 CLI 指南，包括探针配置、进度监控和故障排查。
- [模板检查与请求预览](./template-inspection-and-preview) — 如何查看模板状态并预览最终生效的请求。
