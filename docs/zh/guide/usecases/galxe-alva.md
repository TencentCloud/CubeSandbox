---
title: "Alva：CubeSandbox 落地 GKE 的选型与排障实践"
author: RuiPeng Zhang
date: 2026-10-15
tags:
  - agent
  - gke
  - kubernetes
  - security
lang: zh-CN
---

# Alva：CubeSandbox 落地 GKE 的选型与排障实践

## 业务背景

Alva 是一个金融分析服务，用大模型驱动的 Agent 完成分析工作，Agent 要运行 Python/Shell、安装依赖、执行不完全可信的分析代码。业务运行在海外云上，因此对 CubeSandbox 的完整评估发生在 GKE 上——在现有 sandbox 集群内新增独立 CubeSandbox 专用 Node Pool（n2-standard-8、Ubuntu Containerd、嵌套虚拟化、200GB 独立磁盘），与既有 kata-firecracker 平台共存而不迁移。

## 核心痛点

- **短生命周期负载的固定开销**：既有 Kata 路径按完整 VM 粒度占资源，工作区频繁创建销毁时冷启动占比高。
- **海外云适配成本**：私有镜像仓库认证（Workload Identity/ADC 与 Docker keychain 两套体系）、GKE 网络数据面（kube-dns/NodeLocal DNSCache/Dataplane V2）都不是开箱即用。
- **模板快照的假存活**：guest init 异常退出后构建流程照常继续，快照显示 READY，故障被推迟到 restore 时刻。
- **guest DNS 不通**：指向 kube-dns ClusterIP / NodeLocal DNSCache / Pod 内自建 relay 三层路径全部失败。

## 基于 CubeSandbox 的方案

- **共存式 PoC**：控制面落 main 节点、计算面只允许进新池，独立 label + taint 防误调度；全部 Terraform 变更经 PR + Atlantis 落地；镜像同步内部 Artifact Registry；API/Proxy 走源 CIDR 限制的 Internal LoadBalancer，无公网入口、无长期 JSON key。
- **ADC 凭据修复**（[PR #1544](https://github.com/TencentCloud/CubeSandbox/pull/1544)，已合入上游）：仅当 registry 为 \*.pkg.dev 且无显式凭据时使用 ADC，显式凭据优先，ADC token 走可刷新 token source，失败时区分"权限不足/ADC 不可用/匿名拉取"三种日志。
- **快照前存活判定**（[PR #1541](https://github.com/TencentCloud/CubeSandbox/pull/1541)）：不用绑定创建 RPC context 的 ExitCh/Task.Wait（会产生反向误报），改为 CubeShim 注册 init-task watcher + Cubelet 用独立短超时 context 调 Task.Status，快照前两次 Running 检查加稳定窗口。
- **guest DNS 修复**（[PR #1741](https://github.com/TencentCloud/CubeSandbox/pull/1741)）：当有效 guest resolver 是私有/链路本地/环回 IPv4 时，Cubelet 自动放行该 resolver 的 /32，即使策略是 IP-only；复用 CubeVS 现有 UDP NAT/DNS learning 数据面，不改 CubeVS。

## 效果与收益

- 短任务路径实测（预热模板、5 串行 + 2 并发）：Sandbox.create p50 0.228s，Shell p50 0.103s，Python p50 0.281s，完整生命周期 p50 0.779s，7 次全部成功。
- 三个修复全部回馈上游社区，其中 ADC 与 DNS 修复已合入。
- 沉淀七条海外云落地建议：独立 Node Pool、显式控制镜像架构、第一天就设计 Workload Identity、部署前验证 KVM/存储/CIDR、不把 Helm Ready 当网络兼容通过、Cilium 类数据面单列适配成本、性能 benchmark 排在网络与安全策略之后。

## 参考资料

- 完整案例文章：[沙箱出海记：CubeSandbox 落地 GKE 的选型、排障与性能实录](/zh/blog/posts/2026-10-15-alva-gke)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
