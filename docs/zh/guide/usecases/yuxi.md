---
title: "Yuxi：多租户 Agent 平台的沙箱资源管理实践"
author: 张文杰
date: 2026-11-20
tags:
  - agent
  - multi-tenant
  - pause-resume
  - density
lang: zh-CN
---

# Yuxi：多租户 Agent 平台的沙箱资源管理实践

## 业务背景

Yuxi 是一个可私有部署的多租户知识智能体平台（GitHub 7.5K+ stars），统一了 RAG、知识图谱、多智能体、MCP/Skills、沙盒与权限管理。平台里的 Agent 要装依赖、处理用户文件、执行自己生成的代码，每个用户都需要一个隔离执行环境。此前采用 Docker 容器方案，但无法解决用户环境的长期保留：容器回收则状态丢失，不回收则持续占内存。项目转向 CubeSandbox，目标是「安装的软件和运行状态保留、空闲休眠、任务到来再恢复」。

## 核心痛点

- **休眠到底省了什么不清楚**：全部用户 VM 休眠后 QEMU 仍占约 6 GiB 内存，不知道里面多少可释放。
- **默认调度准入挡住扩容**：默认 `paused_resource_release_ratio=0.0` 使休眠 VM 保留全部额度，v0.7.3 CPU 过滤器还会在剩余额度恰好等于请求量时拒绝调度。
- **预建池的内核开销**：cgroup 预建目录与内核对象随预建规模线性增长。
- **集中恢复能否扛住未知**：百台规模一次性恢复的内存峰值与时延没有实测依据。

## 基于 CubeSandbox 的方案

- **调度准入调优**：将 `paused_resource_release_ratio` 改为 1.0，允许复用休眠额度；十台同时恢复的 Usage 平均耗时约 1.063 秒。
- **缩小 cgroup 预建池**：调整后 cgroup 目录从 6076 个降到 167 个，SUnreclaim 从 626596 KiB 降到 167832 KiB。
- **空闲页上报回收**：`virtio-balloon-pci,free-page-reporting=on` + `memory-backend-memfd` `share=on`；上报粒度从 2 MiB 降到 4 KiB 时 RSS 从 5.725 GiB 降到 4.880 GiB，持久粒度选 8 KiB。
- **驻留归因方法**：关联 guest `/proc/kpageflags` 与 QEMU `/proc/PID/pagemap` 按地址映射分类，识别「guest 已空闲、宿主仍驻留」的页（2.293 GiB）。
- **生命周期控制**：timeout -1、`onTimeout: pause` + `autoResume: false`，worker 按最后活动时间判断休眠并主动恢复；适配层只用只读接口，监控不触发恢复。

## 效果与收益

- 220 次真实 Agent 任务（110 已有用户恢复 + 55 旧用户再恢复 + 55 新用户首次创建）全部完成，无 OOM、无 502、未触容器上限。
- VM 创建/恢复平均 0.18–0.27 秒（P95 400 ms），任务总耗时均值 19–22 秒。
- 页面组成实测：文件缓存 5.423 GiB、guest 已空闲但宿主仍驻留 2.293 GiB；粒度临时降到 4 KiB 后 RSS 从 10.60 GiB 降到 8.32 GiB。
- 形成 100 台轻量 VM 的部署依据：QEMU 八核/24 GiB、容器八核/32 GiB、实例上限 512、创建并发 4、释放比例 1.0。

## 参考资料

- 完整案例文章：[多租户 Agent 平台的沙箱资源管理实践：调度准入、空闲页回收与百台密度验证](/zh/blog/posts/2026-11-20-yuxi)
- Yuxi 项目：[xerrors/Yuxi](https://github.com/xerrors/Yuxi)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
