---
title: 计算节点在线却被判 HeartbeatExpired（时钟偏移）
author: Cold-E
date: 2026-10-22
tags:
  - troubleshooting
lang: zh-CN
---

# 计算节点在线却被判 HeartbeatExpired（时钟偏移）

## 症状

`cubeopscli node ls`（或 WebUI `/nodes` 页面）显示某计算节点 `Healthy=False`、`UnhealthyReason=HeartbeatExpired`，但该节点其实正常在线——`cubelet` 在运行、心跳也在上报。本应调度到该节点的沙箱创建失败，报 `no ready replica` / `no more resource`，因为调度器会过滤掉 `HeartbeatExpired` 节点。

关键判据：该节点的 `MetaDataUpdateAt` 时间戳一直在推进（心跳**确实**在到达），但节点仍被判过期。

## 环境

- CubeSandbox v0.7.x，多节点部署（控制节点与计算节点分属不同主机）
- 计算节点时钟未同步到统一时间源（NTP/chrony）
- 相关组件：`cubelet`（心跳上报）、`cube-ops`（健康判定）

## 根因

`cubelet` 每 ~1–2s 向 `cube-ops` 上报一次心跳（`POST /internal/v1/node-agent/nodes/<ip>/status`），其中 `HeartbeatTime` 用**计算节点本地 wall clock** 打戳。`cube-ops` 用**自己**本地 wall clock 作为 `now` 判定健康：

```go
// CubeOps/internal/nodemanagement/service/health.go
func EvaluateHealth(reportedReady bool, heartbeatTime, now time.Time, timeout time.Duration) HealthStatus {
    if heartbeatTime.IsZero() || now.Sub(heartbeatTime) > timeout {
        return HealthStatus{Healthy: false, UnhealthyReason: model.ReasonHeartbeatExpired}
    }
    ...
}

// MetadataTimeout 返回 cubelet 心跳过期窗口（15s）
func MetadataTimeout() time.Duration { return 15 * time.Second }
```

窗口是 15s。如果计算节点时钟比控制节点慢超过 15s，它打的 `HeartbeatTime` 在 `cube-ops` 看来已经超过 15s 前——于是每条刚到的心跳"一到达就过期"。心跳流量本身没问题，只是两端时钟不一致。

这不是代码 bug，而是分布式系统的标准前提：所有节点共享同一时间基准。`chrony` 未配置 server 的计算节点（`Stratum=0`、`Reference ID=00000000`）会自由漂移，VM/PVM 计算节点尤其容易漂。

## 诊断

1. 查看节点状态与原因：

   ```bash
   cubeopscli node ls
   # 关注：Healthy=False, UnhealthyReason=HeartbeatExpired, 且 MetaDataUpdateAt 较新
   ```

2. 区分"时钟偏移"与"真离线"。**若 `MetaDataUpdateAt` 几天没变**，是 `cubelet` 真的停了——那是真离线，不是本问题。**若 `MetaDataUpdateAt` 一直在更新**但节点仍 `HeartbeatExpired`，就是两端时钟不一致。

3. 在计算节点上检查 chrony 是否同步：

   ```bash
   chronyc tracking
   # Stratum: 0 且 Reference ID: 00000000 => 未同步，自由运行
   ```

4. 测量计算节点与控制节点的时钟差，最简单的办法是直接比对 wall clock：

   ```bash
   # 在计算节点记下时间，再 SSH 到控制节点比对：
   date +%s          # 计算节点
   ssh <control-node-ip> date +%s   # 控制节点
   # 差值大于 15s 即可复现误判 HeartbeatExpired
   ```

   若控制节点是 chrony server，可直接测量：

   ```bash
   chronyc -h <control-node-ip> tracking   # 看 offset 字段
   ```

## 修复

将计算节点时钟同步到**与控制节点相同**的时间源。

1. 确认时间源。若控制节点以 chrony 作本地 NTP server（如 `local stratum 10` + `allow`），用它；否则用公网 NTP pool 或企业内网 NTP server。务必选与控制节点一致的时间源，两端时钟才能对齐。

2. 在计算节点配置 chrony，先删掉旧的 `server`/`pool` 行避免冲突：

   ```bash
   # 示例：公网 NTP pool
   sudo sed -i '/^server /d; /^pool /d' /etc/chrony.conf
   echo 'server pool.ntp.org iburst' | sudo tee -a /etc/chrony.conf
   # 或用内网 NTP server：
   # echo 'server <your-ntp-server> iburst' | sudo tee -a /etc/chrony.conf
   ```

3. 重启 chrony 并确认 offset 收敛到接近 0：

   ```bash
   sudo systemctl restart chronyd
   chronyc tracking   # Stratum 应为非 0，offset 应接近 0
   ```

4. 节点会在下一个心跳周期内变为健康，验证：

   ```bash
   cubeopscli node ls   # 修复的节点 Healthy=True
   ```

无需重启 `cubelet`/`cube-ops`、无需改配置——两端时钟对齐后，现有心跳流即被判为新鲜。

## 参考

- 心跳判定：`CubeOps/internal/nodemanagement/service/health.go`（`EvaluateHealth`、`MetadataTimeout`）
- 心跳上报：`Cubelet/pkg/cubelet/node_status.go`（`HeartbeatTime: kl.clock.Now()`）
- 相关：#455（stale node health 处理——本问题是另一种失败模式：心跳按时到达，但两端时钟不一致）
