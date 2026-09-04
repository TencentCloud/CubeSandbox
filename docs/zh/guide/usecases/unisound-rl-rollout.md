---
title: "云知声：RL rollout 场景下的密度边界压测"
author: 云知声 Atlas 智算团队
date: 2026-09-01
tags:
  - agent
  - rl
  - rollout
  - density
lang: zh-CN
---

# 云知声：RL rollout 场景下的密度边界压测

## 业务背景

云知声 Atlas 智算团队用 Cube Sandbox 支撑 Agent 轨迹 rollout，覆盖两类负载：SWE Agent 数据合成（批量解决真实代码仓库问题、产出训练数据）和 Agent RL 环境训练（大量并行 episode 与环境交互）。每条轨迹需要干净、隔离、可复现的执行环境，生命周期分钟级，规模并行，且沙箱里运行的是模型生成的不可信代码。

## 核心痛点

- **安全隔离是硬指标**：沙箱内代码行为不可预判（死循环、内存泄漏、fork 炸弹、异常网络调用），namespace/Cgroup 级隔离存在逃逸风险。
- **吞吐优先于稳态**：单节点每秒需要处理几十到上百个创建销毁请求。
- **密度边界不可拍脑袋**：标称规格落到真实负载上，受资源超卖、运行时峰值、调度参数多重影响。
- **环境一致性**：E2B SDK 默认 `bash -lc` 拉起登录 shell，两阶段任务的环境变量不会自动延续。

## 基于 CubeSandbox 的方案

- **MicroVM 硬件级隔离**：每个沙箱是独立 KVM MicroVM，拥有自己的内核，满足不可信代码执行场景。
- **控制面/计算面分离**：1 个控制节点（cube-api / CubeMaster / cube-lifecycle-manager / cube-proxy / coredns / webui / MySQL / Redis）+ N 个轻量计算节点（仅 network-agent + cubelet），计算节点通过 `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` 注册。
- **模板主动分发**：CubeMaster 把 OCI 镜像烤成 ext4 rootfs 后推送 replica 到各计算节点，本地取内存快照，起沙箱走纯本地 FICLONE 克隆、零网络 IO；`/data/cubelet` 必须 XFS + reflink。
- **密度推导三步法**：按沙箱规格（1 核 / 4096 MiB / 10 GiB 可写层）与调度参数（内存预留 10 GiB、内存超卖 2 倍、CPU 超卖 3 倍封顶 80%）推算理论上限 117；区分安全边界（58，不超卖）与账面天花板（117）的含义；实测稳态 80-100。
- **E2B SDK 直接接入**：训练侧调度代码无需特殊适配；两阶段任务之间显式重设环境变量，绕开 login shell 状态不延续的问题。

## 效果与收益

- **三组密度数字**：单节点调度上限 117、不超卖安全边界 58、实测稳态 80-100（双节点 160-200），瓶颈在内存侧。
- **三类瓶颈的识别方法**：内存超卖兑现（宿主 OOM）、磁盘水位超 80% 被移出调度（报错码 130597，节点仍显示 HEALTHY）、创建突发卡在 network-agent 分配 tap 设备（约 20 路并发创建为界）——三类瓶颈压力模型不同，需分开定位。
- **AutoPause 杠杆评估**：`paused_resource_release_ratio` 调到 0.7-0.8 理论上账面密度可达 3-4 倍（单节点 350-470），但 resume 变为 best-effort（瞬时资源不足返回 409）；在训练侧容错就绪前保持默认 0.0，参数支持热加载可随时再评估。

## 参考资料

- 完整案例文章：[云知声工程实践：RL rollout 场景下的 CubeSandbox 密度边界压测](/zh/blog/posts/2026-09-01-unisound-rl-rollout)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
