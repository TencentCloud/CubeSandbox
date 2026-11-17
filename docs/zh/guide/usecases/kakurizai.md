---
title: "KakuriZai：多 Agent 并行编辑的轻量隔离工作区"
author: 佐藤 良（mizuamedesu）
date: 2026-11-06
tags:
  - agent
  - overlayfs
  - snapshot
  - host-mount
lang: zh-CN
---

# KakuriZai：多 Agent 并行编辑的轻量隔离工作区

## 业务背景

KakuriZai 是日本筑波大学 IPA-CyberLab 实验室的沙箱生命周期控制平面项目（附 Studio Web 界面），CubeSandbox 是它引入的沙箱运行时之一。目标是让多个 Agent 并行编辑 UE 这类连源码带二进制的巨型项目：一方做功能追加，另一方尝试另一种实现，最后查看各自变更、把需要的部分采纳回原始源码。

## 核心痛点

- **并行编辑冲突**：多个 Agent 在同一项目里同时工作，文件变更相互冲突。
- **巨型项目无法整份复制**：UE 级项目连源码带二进制，为每个 Agent 复制完整环境不现实。
- **模板与磁盘容量不匹配**：从模板派生沙箱时改磁盘大小会阻塞在快照 resize（`'rootfs-gen0' is a snapshot; cannot resize`）。
- **overlay 删除语义的陷阱**：kernel overlayfs 的 whiteout（.wh. 前缀）与 unionfs-fuse（`_HIDDEN~` 后缀）表现形式不一致，直接复制 upper 会丢删除记录。
- **VM 快照不含 host-mount 数据**：lower/upper 在宿主机一侧，VM 快照未必能作为独立备份保存。

## 基于 CubeSandbox 的方案

- **lower/upper 分层共享**：宿主源码作为只读 lower 被所有工作环境共享，每个 World（工作环境）只持有自己的可写 upper，guest 内叠加呈现为 /workspace；copy-up、whiteout、apply 全套规则支撑"各改各的、按需采纳回源"。
- **多挂载后端**：setupOverlay() 按 kernel overlayfs → unionfs-fuse → fuse-overlayfs 顺序尝试；挂载点强制限定在 /workspace 之下；变更收集侧适配 unionfs-fuse whiteout 读取。
- **模板资源匹配**：resolveTemplateForResources() 核对磁盘/CPU/内存/instance type/network type 五项，不匹配时复用或异步新建兼容模板（轮询上限 20 分钟）。
- **降级策略**：宿主机无 cubecli 时 World 创建不失败，运行时状态标记 planned，待 Cube 可用后再真正创建。
- **网络与进阶用法**：network_type=tap + iptables 链（KAKURIZAI-EGRESS/INGRESS）出站管控；多沙箱 K8s 实验室批量创建；agctl replicate stateful 模式带状态分叉——有 host mount 时先把 /workspace 内容复制进 guest 再纳入快照。

## 效果与收益

- 多个 Agent 可并行编辑同一巨型项目且互不串改，公共大文件始终保持引用。
- 三种挂载方式（agctl-overlay / cubesandbox-readonly / unsafe-rw）按用途可选。
- 小型 Cube 沙箱组成的 K8s 集群可像单个 World 一样创建管理。

## 参考资料

- 完整案例文章：[用 CubeSandbox 打造轻量隔离工作区：多 Agent 并行编辑的分层实践](/zh/blog/posts/2026-11-06-kakurizai-workspaces)
- KakuriZai 项目：[IPA-CyberLab/IPA-RS-KakuriZai](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
