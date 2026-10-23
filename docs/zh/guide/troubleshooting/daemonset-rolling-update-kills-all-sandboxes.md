---
title: 滚动更新 cube-node DaemonSet 会连带杀掉本节点全部沙箱
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: zh-CN
---

# 滚动更新 cube-node DaemonSet 会连带杀掉本节点全部沙箱

## 问题现象

Kubernetes 形态下，任何导致 `cube-node` Big Pod 重建的操作（`helm upgrade` 改动 pod spec、`kubectl set image`、节点重调度）都会**无提示地杀掉该节点上所有正在运行的沙箱**，全部 VM 同时消失。之后 `GET /sandboxes` 仍把它们报为 `running`——脏条目，永远不会收敛。我们在滚动一个 exec sidecar 时连踩两次，每次都灭掉节点上全部生产沙箱。

## 环境信息

- Cube Sandbox 版本：v0.7.3（chart cube-0.7.3），v0.7.0 时代的 K8s 形态即存在
- 部署形态：Kubernetes，`cubeNode.hostNetwork: true`，chart 默认 `security.hostPID: true`（线上核实）
- 主机 OS / 内核：CentOS Stream 9，6.x 内核
- 相关组件：cube-node DaemonSet、containerd-shim-cube-rs

## 根因分析

精确的杀灭路径需要维护者确认——生产环境实证的是结果与触发条件。朴素的"PID namespace 拆除"理论在此**不成立**：chart 以 `security.hostPID: true` 运行 Big Pod，shim 与宿主共享 PID namespace，仅凭 pod 重建杀不掉它们。

我们实际观察到的：pod 替换期间，新容器的 `component-entrypoint.sh` 会终止先前的节点组件（在 shim 消失的同一时刻，cubelet 日志里捕获到 `component-entrypoint.sh: line 1082: ... Killed "${bin}"`）。在 `hostPID: true` 下，对进程组的 kill 或对旧组件进程树的显式清扫会波及节点上每个 `containerd-shim-cube-rs`。值得注意的是，官方升级路径（`docs/guide/kubernetes/upgrade.md`）把 shim/kernel/guest 升级路由到 `cube-node-installer` DaemonSet，恰好不重建 Big Pod——重建它似乎正是发生此事的未支持路径。shim 死后，脏 `running` 条目无任何收敛（与 #1958 同族）。

## 解决方案

1. **优先走受支持路径**：节点侧制品（shim/kernel/guest）升级经 `cube-node-installer` DaemonSet，不重建 Big Pod（`docs/guide/kubernetes/upgrade.md`）。只有 cubelet/runtime 镜像变更会重建它，且按升级指南要求先 drain。
2. 确实要重建 Big Pod 时，按破坏性操作对待：事先记录每个沙箱的 templateID / volumeMounts / timeout；事后 `DELETE` 脏 `running` 条目并按原参数重建（卷数据保留，guest 内存态丢失）。
3. 我们的部署里，自有 sidecar 的迭代走自替换（`/self-upgrade` 式）端点、从不重建 pod——这是我们的运维选择，不是上游机制。
4. 前后验证：
   ```sh
   ps aux | grep containerd-shim | grep -oE '\-id [0-9a-f]{8}' | sort
   ```
   与沙箱清单对照。

## 参考资料

- 相关 issue：#1958（同族 pod 重建问题，eBPF 端口映射）
- 相关 issue：#1890（create 失败后的 shim 生命周期）
