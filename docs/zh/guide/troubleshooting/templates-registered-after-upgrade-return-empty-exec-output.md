---
title: 升级后注册的模板：沙箱 exec 返回空输出（exit 0、stdout 为空）
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: zh-CN
---

# 升级后注册的模板：沙箱 exec 返回空输出（exit 0、stdout 为空）

## 问题现象

从 v0.7.0 升级到 v0.7.3 后，**升级前注册的模板一切正常，升级后新注册的模板**开出的沙箱：命令实际已执行（exit code 是真实的），但经 containerd task exec（containerd v2 client + `cio` 流收集）回读 **stdout/stderr 永远为空**——约 40ms 返回 `exit_code:0, stdout:""`。同节点同时间窗：v0.7.0 shim 上的沙箱完全正常，唯一变量是 shim 二进制。

## 环境信息

- Cube Sandbox 版本：v0.7.0 → v0.7.3（Kubernetes 形态）
- 部署形态：Kubernetes chart，`component_versions/cube-shim/` 下两版 shim 并存
- 主机 OS / 内核：CentOS Stream 9，6.x 内核
- 相关组件：CubeShim（containerd-shim-cube-rs）、cube-agent

## 根因分析

已实证的是相关性：出问题的沙箱恰好是模板快照钉在 v0.7.3 shim（升级后注册）的那批，且只换 shim 二进制（见下）即恢复回读——同节点同窗口的对照实验里，shim 二进制是唯一变量。

精确的内部机制仍待维护者确认。我们最初的解读（从内存快照复活的 agent 不走 CreateContainer，因此 agent/src/rpc.rs 的 `reset_log_stream_readers` 修复永不触发）**不完整**：模板恢复确实会带着 `cube.appsnapshot.container.id` 标记走到 CreateContainer（Cubelet/plugins/cbri/.../cubebox.go），reset 正是在那里被调用的。v0.7.3 快照/恢复路径中仍有某种东西让日志流回读失效——我们报告症状、对照实验与规避方法，精确断点留给 #2015。

注意产品 exec API 本身不受影响：CubeMaster `/cube/sandbox/exec` 是 fire-and-forget（dummy IO），我们验证过命令确实在 guest 内执行。坏的是经 containerd task exec 的输出回读——外部工具可能依赖这一行为。

## 解决方案

对 shim 槽位做受控降级——把 v0.7.3 二进制换回 v0.7.0，其余 v0.7.3 组件不动：

```sh
cd /data/cubelet/root/component_versions/cube-shim/v0.7.3/bin
cp containerd-shim-cube-rs containerd-shim-cube-rs.v073.bak
# 替换文件必须先放进目标目录再 mv:同文件系统的 rename(2) 才是原子的;
# 经 /tmp(常见 tmpfs)staging 会让 mv 退化为 copy+unlink 重新踩 ETXTBSY,
# 还会带上 /tmp 的 SELinux label。
cp ../v0.7.0/bin/containerd-shim-cube-rs ./.stage && chmod +x ./.stage
mv ./.stage containerd-shim-cube-rs
```

**注意事项（明确代价）：**
- 已跑沙箱持有旧 inode 不受影响；新沙箱获得 v0.7.0 shim 行为。已在生产混合负载下验证。
- 快照把 shim 与**成对的 cube-runtime** 绑定为一组（`cubebox.proto`："cube-shim (and sibling cube-runtime) version bound..."）。本方案执行后，"v0.7.3" 槽位内是 v0.7.0 shim + v0.7.3 cube-runtime，`version.json` 仍报 v0.7.3——刻意的混装槽位，此处实践可用，但不会自愈、会干扰版本上报。把它当作缺陷修复前的过渡，不是终态。
- 节点制品重分发可能还原原二进制；重分发后需重放。

同场排查提示：GitHub compare API 有 300 文件截断——核对 tag 间 CubeShim/hypervisor 变更用 tree sha 或源码 diff，别信 compare 文件列表。

## 参考资料

- 相关代码：agent/src/rpc.rs（`reset_log_stream_readers`）、CubeShim/shim/src/container/exec.rs
- 相关 issue：#1616（模板创建后的 RPC log forward）
