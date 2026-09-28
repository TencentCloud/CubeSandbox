---
title: "openFuyao：Kubernetes 中运行 CubeSandbox 的落地实践"
author: 王文涛、李爱华
date: 2026-09-17
tags:
  - kubernetes
  - deployment
  - ebpf
  - production
lang: zh-CN
---

# openFuyao：Kubernetes 中运行 CubeSandbox 的落地实践

## 业务背景

openFuyao 是面向通算和智算集群软件技术创新的开源社区，算力基础设施统一构建在 K8s 上。openFuyao Agent 沙箱 SIG 团队把 CubeSandbox 的控制面、数据面、计算面全部封装成标准 K8s workload，实现"K8s 管节点，CubeSandbox 管沙箱"，方案已作为 [discussion #636](https://github.com/TencentCloud/CubeSandbox/discussions/636) 提交社区。v0.6.0 官方 K8s 原生部署方案与其自研模式类似，可平滑切换。

## 核心痛点

- **两套运维体系**：CubeSandbox 早期物理机部署形态（systemd + Docker Compose）与 K8s 平台并存，发布、监控、证书轮转各走各的通道，心智成本双倍。
- **同节点网络不通**：计算面跑进 Big Pod 后，同节点访问沙箱映射端口必然失败（issue [#443](https://github.com/TencentCloud/CubeSandbox/issues/443)）。
- **cubelet 进程模型与 PID 1 冲突**：cubelet 假设自己跑在宿主机 systemd 下，"进程 B 杀父成孤儿"的模型进 pod 后会让容器崩溃。
- **特性语义漂移**：host-mount 的"宿主"从节点漂移到 Big Pod；CubeVS 默认拒绝的 CIDR 列表恰好覆盖全部 K8s pod/cluster 网段。
- **双调度器冲突**：K8s 调 pod、CubeMaster 调沙箱，pod resources.limits 卡死会让 Cube 的 overcommit 被 cgroup 反噬。

## 基于 CubeSandbox 的方案

- **Big Pod 部署架构**：cubelet + network-agent + cube-egress + CubeShim + CubeHypervisor 封装为特权 DaemonSet（hostPath /dev/kvm、/data/cubelet，沙箱跑在 Big Pod 内）；CubeAPI/CubeMaster/CubeProxy 走 Deployment+Service（CubeMaster 挂 PV 存模板）；MySQL/Redis 走 StatefulSet；CoreDNS 复用集群配 cube.app 转发。
- **网络问题定位链路**：5 个对照场景锁定"同节点入向流量 + 映射端口"必不通 → nettrace/TAP 抓包 → bpftool → bpf_printk 日志，根因为 eBPF 本节点转发缺伪首部导致 cksum 校验失败；该 issue 推动上游定位，修复在 v0.4.0 完成（PR [#469](https://github.com/TencentCloud/CubeSandbox/pull/469) 引用了 issue #443）。
- **孤儿进程解法（不改一行 cubelet 代码）**：tini 作 PID 1 + bash entrypoint 后台起 cubelet + pidfile + preStop hook，完整保留 cubelet 原进程模型。
- **四处语义平移经验**：CubeMaster 挂 PV + 本地模板持久化 hostPath（跟随节点不跟随 pod）；计算面 pod 需 hostPath 卷 + 特权模式穿透真节点目录；按需 allow_out 显式放行集群内 service CIDR；network-agent 用"路由表解析网关 IP → 邻居表按 IP 精确匹配 → 接受 NUD_STALE/NUD_PROBE 宽松过滤"解决 pod 内拿不到网关 MAC。
- **调度分工**：K8s 调度限制在节点级，沙箱装箱交给 CubeMaster overcommit；计算面 pod 的 CPU/内存 limit 给足或不限。

## 效果与收益

- 沙箱计算节点像普通工作负载一样被 K8s 弹性扩缩、滚动升级，不侵入 APIServer/Scheduler/etcd，只用标准原语。
- 深度使用 template 分发、snapshot、pause-resume、host-mount、egress、网络策略、E2B 接口、ARM 适配等特性。
- issue #443 被上游 v0.4.0 修复 PR 引用，成为"实践反馈上游"的典型案例。
- 向社区提出 5 项能力诉求：跨机暂停恢复、跨机 Snapshot 启动（v0.7.0 已支持预览版）、控制面/数据面隔离与无断服升级、更高创建吞吐、兼容 cilium/calico 等 eBPF CNI。

## 参考资料

- 完整案例文章：[Kubernetes 中运行 CubeSandbox 的部署与落地应用实践](/zh/blog/posts/2026-09-17-openfuyao)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
