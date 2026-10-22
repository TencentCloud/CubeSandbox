---
title: 升级后注册的模板：沙箱 exec 返回空输出（exit 0、stdout 为空）
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: zh-CN
---

# 升级后注册的模板：沙箱 exec 返回空输出（exit 0、stdout 为空）

## Symptom

从 v0.7.0 升级到 v0.7.3 后，**升级前注册的模板一切正常，升级后新注册的模板**开出的沙箱：命令实际已执行（exit code 是真实的），但经 containerd task exec（containerd v2 client + `cio` 流收集）回读 **stdout/stderr 永远为空**——约 40ms 返回 `exit_code:0, stdout:""`。同节点同时间窗：v0.7.0 shim 上的沙箱完全正常，唯一变量是 shim 二进制。

## Environment

- Cube Sandbox 版本：v0.7.0 → v0.7.3（Kubernetes 形态）
- 部署形态：Kubernetes chart，`component_versions/cube-shim/` 下两版 shim 并存
- 主机 OS / 内核：CentOS Stream 9，6.x 内核
- 相关组件：CubeShim（containerd-shim-cube-rs）、cube-agent

## Root Cause

模板内存快照携带 shim 版本标记：升级前模板钉在 v0.7.0（其沙箱仍派 v0.7.0 shim），升级后注册的模板标记 v0.7.3、撞上新快照机制。从内存快照复活的 guest agent 不走 CreateContainer 路径，agent 侧的 `reset_log_stream_readers` 修复（agent/src/rpc.rs）因此永不触发——日志流读端失效，shim 侧转发器在空读上空转。注意产品 exec API 本身没坏：CubeMaster `/cube/sandbox/exec` 是 fire-and-forget（dummy IO，不收集输出）；坏的是"从恢复的 agent 回读输出"这个产品从未依赖的行为。

## Resolution

对 shim 槽位做受控降级——把 v0.7.3 二进制换回 v0.7.0，其余 v0.7.3 组件全部不动：

```sh
cd /data/cubelet/root/component_versions/cube-shim/v0.7.3/bin
cp containerd-shim-cube-rs containerd-shim-cube-rs.v073.bak
cp ../v0.7.0/bin/containerd-shim-cube-rs /tmp/.s && chmod +x /tmp/.s
mv /tmp/.s containerd-shim-cube-rs   # shim 运行中直接 cp 会报 "Text file busy"
```

已跑沙箱不受影响（持有旧 inode）；新沙箱全部回落 v0.7.0 shim。验证：用升级后注册的模板开一个沙箱，exec 输出恢复。排查提示：GitHub compare API 有 300 文件截断，核对 tag 间 CubeShim/hypervisor 变更要用 tree sha 或源码 diff，别信 compare 文件列表。

## References

- 相关代码：agent/src/rpc.rs（`reset_log_stream_readers`）、CubeShim/shim/src/container/exec.rs
- 相关 issue：#1616（模板创建后的 RPC log forward）
