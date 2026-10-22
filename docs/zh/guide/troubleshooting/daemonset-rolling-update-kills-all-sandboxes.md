---
title: 滚动更新 cube-node DaemonSet 会连带杀掉本节点全部沙箱
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: zh-CN
---

# 滚动更新 cube-node DaemonSet 会连带杀掉本节点全部沙箱

## Symptom

Kubernetes 形态下，任何导致 `cube-node` Big Pod 重建的操作（`helm upgrade` 改动 pod spec、`kubectl set image`、节点重调度）都会**无提示地杀掉该节点上所有正在运行的沙箱**，全部 VM 同时消失。之后 `GET /sandboxes` 仍把它们报为 `running`——脏条目，永远不会收敛。我们在滚动一个 exec sidecar 时连踩两次，每次都灭掉节点上全部生产沙箱。

## Environment

- Cube Sandbox 版本：v0.7.3（chart cube-0.7.3），v0.7.0 时代的 K8s 形态即存在
- 部署形态：Kubernetes，`cubeNode.hostNetwork: true`
- 主机 OS / 内核：CentOS Stream 9，6.x 内核
- 相关组件：cube-node DaemonSet、containerd-shim-cube-rs

## Root Cause

`containerd-shim-cube-rs`（内嵌 VMM）在宿主 `ps` 里可见，但它运行在 cubelet 容器的 **PID namespace** 里。Pod 重建时 kubelet 拆除整个 PID namespace，里面的所有进程——包括每个沙箱的 shim/VM——被 SIGKILL。K8s 形态下沙箱生命周期与 pod 生命周期耦合是设计使然。与 #1958（pod 重建后 eBPF 端口映射残留）同族：都是 pod 生命周期与宿主状态耦合、且没有收敛路径。

## Resolution

1. **红线**：生产集群绝不滚动 cube-node DS——节点侧组件迭代一律热更（新二进制放宿主路径，进程自替换重启，pod 不动）；
2. 必须滚动时（版本升级），按计划窗口执行并预设重建流程：
   - 事先记录每个沙箱的 templateID / volumeMounts / timeout；
   - 滚动后 `DELETE` 脏 `running` 条目，按原参数 `POST /sandboxes` 重建；卷数据保留，guest 内存态丢失。
3. 前后验证：
   ```sh
   ps aux | grep containerd-shim | grep -oE '\-id [0-9a-f]{8}' | sort
   ```
   与沙箱清单对照。

## References

- 相关 issue：#1958（同族 pod 重建问题，eBPF 端口映射）
- 相关 issue：#1890（create 失败后的 shim 生命周期）
