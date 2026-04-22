---
layout: home

hero:
  name: "Cube Sandbox"
  text: "Empowering your AI Agents."
  tagline: "极速启动、高并发、安全且轻量化的 AI Agent 沙箱。"
  actions:
    - theme: brand
      text: 快速开始
      link: /zh/guide/quickstart

features:
  - title: "⚡ 极速启动"
    details: 资源池化预置 + 快照克隆，跳过所有冷启动开销。创建沙箱比一次眨眼都快。
  - title: "🔒 硬件级隔离"
    details: 每个沙箱配备独立操作系统内核，运行在专属 MicroVM 中。
  - title: "🔌 E2B 生态兼容"
    details: 完全兼容 E2B SDK 接口，替换一个环境变量即可从 E2B 云无缝切换，零业务代码改动。
  - title: "📦 高密度部署"
    details: 单沙箱额外开销仅 MB 级，通过内核共享与写时复制（CoW），单机可运行数千个实例。
  - title: "🛡️ 网络安全"
    details: 基于 eBPF 的 CubeVS 在内核态实现严格的沙箱间网络隔离，支持细粒度出站流量过滤策略。
  - title: "📸 灵活的状态管理"
    details: 支持 Checkpoint 保存、任意状态回滚与快速分叉，为并行开发和多版本测试提供前所未有的灵活性。（即将开源）
---

## 开始使用

- [快速开始](./guide/quickstart.md) — 几分钟内从零到运行沙箱
- [本地构建部署](./guide/self-build-deploy.md) — 从源码构建并在单机上部署
- [多机集群部署](./guide/multi-node-deploy.md) — 扩展到多节点集群
- [架构概览](../architecture/overview.md) — 了解系统设计与核心组件

## 场景示例

SDK 示例与端到端场景：

- [示例项目](./guide/tutorials/examples.md) — 代码执行、浏览器自动化、OpenClaw 集成、RL 训练等场景
- [仓库示例合集](https://github.com/tencentcloud/CubeSandbox/tree/master/examples) — GitHub 上的完整示例
