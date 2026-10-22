---
title: 计算节点在线却被判 HeartbeatExpired（时钟偏移）
author: Cold-E
date: 2026-10-22
tags:
  - heartbeat
  - clock
  - chrony
  - cube-ops
lang: zh-CN
---

# 计算节点在线却被判 HeartbeatExpired（时钟偏移）

## 症状

`cubeopscli node ls --json`（WebUI `/nodes` 页面只用橙点标记该节点——不渲染状态文字、也不显示原因）显示某计算节点 `Healthy=false`、`UnhealthyReason=HeartbeatExpired`，但该节点其实正常在线——`cubelet` 在运行、心跳也在上报。本应调度到该节点的沙箱创建失败，报 `no ready replica` / `no more resource`，因为调度器会过滤掉 `HeartbeatExpired` 节点。

关键判据：该节点的 `MetaDataUpdateAt` 时间戳一直在推进（心跳**确实**在到达），但节点仍被判过期。

## 环境

- CubeSandbox v0.7.x，多节点部署（控制节点与计算节点分属不同主机）
- 计算节点时钟未同步到统一时间源（NTP/chrony）
- 相关组件：`cubelet`（心跳上报）、`cube-ops`（健康判定）

## 根因

`cubelet` 每秒左右向 `cube-ops` 上报一次心跳（`POST /internal/v1/node-agent/nodes/<node-id>/status`；`node_status_update_frequency` 在 `Cubelet/config/config.toml` 默认 `1s`；`<node-id>` 是节点标识——K8s chart 下是节点名，one-click/裸金属路径下通常是配置的 node ID），其中 `HeartbeatTime` 用**计算节点本地 wall clock** 打戳。`cube-ops` 用**自己**本地 wall clock 作为 `now` 判定健康：

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

1. 查看节点状态与原因。默认 `node ls` 表只打印 `NODE_ID / NODE_IP / INSTANCE_TYPE / ZONE / CPU_TYPE / HEALTHY / SCHEDULING_DISABLED / HOST_STATUS`——**不**显示 `UnhealthyReason` 和心跳时间戳，需用 `--json`（完整字段，两者都能看）或 `--score-only`（切到含 `METADATA_UPDATE` 的分数/时间视图，但会去掉 `HEALTHY`）：

   ```bash
   cubeopscli node ls --json          # 完整字段：Healthy, UnhealthyReason, MetaDataUpdateAt
   # 或，观察心跳时间戳是否在推进（此视图去掉 HEALTHY/UnhealthyReason）：
   cubeopscli node ls --score-only    # NODE_ID / SCORE / METRIC_UPDATE / METRIC_LOCAL_UPDATE / METADATA_UPDATE
   # 关注：Healthy=false, UnhealthyReason=HeartbeatExpired, 且 MetaDataUpdateAt 较新
   #（--json 是能同时看到原因和时间戳的模式）
   ```

2. 区分"时钟偏移"与"真离线"。**若 `MetaDataUpdateAt` 几天没变**，是 `cubelet` 真的停了——那是真离线，不是本问题。**若 `MetaDataUpdateAt` 一直在更新**但节点仍 `HeartbeatExpired`，就是两端时钟不一致。

   注意这个比较是**相对的**：`EvaluateHealth` 用 `cube-ops` 的 wall clock（控制节点上 `now = time.Now()`）做 `now.Sub(heartbeatTime)`，只有当 heartbeat 时间在控制节点时钟看来是**过去**——即计算节点时钟**落后于**控制节点——才触发。任意一端失步都可能造成这个相对条件：计算节点漂慢，或控制节点跳快（控制节点自己的 chrony 未同步、或 VM pause/resume）。症状本身是单向的（计算节点偏快则保持健康），但任一时钟都可能是相对偏移的来源，所以两端都要查——先查计算节点，若正常再查**控制**节点（各自跑 `chronyc tracking`）。

3. 在计算节点上检查 chrony 是否同步：

   ```bash
   chronyc tracking
   # Stratum: 0 且 Reference ID: 00000000 => 未同步，自由运行
   # 若此处计算节点看着正常但节点仍 HeartbeatExpired，
   # 在控制节点上也跑一次同样的检查——它可能向前跳了。
   ```

4. 测量计算节点与控制节点的时钟差，最简单的办法是直接比对 wall clock：

   ```bash
   # 在计算节点记下时间，再 SSH 到控制节点比对：
   date +%s          # 计算节点
   ssh <control-node-ip> date +%s   # 控制节点
   # 计算节点必须落后于控制节点：EvaluateHealth 检查的是
   # `now.Sub(heartbeatTime) > timeout`，只有计算节点时钟偏慢才会
   # 复现 HeartbeatExpired。计算节点偏快则保持健康（delta 为负）。
   # 在该方向上差值大于 15s 即可复现误判 HeartbeatExpired。
   ```

   若要对指定 NTP 源做一次性偏移测量，在**计算节点**上跑（打印 `System clock ... wrong by <N> seconds`，不修改时钟）。目标必须真的提供 NTP 服务——用控制节点所同步的同一个 `<your-ntp-server>`（见修复步骤 1）；若控制节点不是 NTP server，改指向一个真正的 NTP 源：

   ```bash
   chronyd -Q 'server <your-ntp-server> iburst'   # 一次性探测，打印相对该 server 的偏移
   ```

## 修复

两端都必须同步到同一时间源——这个比较是相对的，若跳快的是控制节点的时钟，只修计算节点无法恢复该节点。先确认两端时钟都已同步（诊断步骤 3 在两个节点上各跑一次），再同步偏移的那一端。

1. 确认时间源。若控制节点以 chrony 作本地 NTP server（如 `local stratum 10` + `allow`），用它；否则用公网 NTP pool 或企业内网 NTP server。务必选与控制节点一致的时间源，两端时钟才能对齐。若控制节点自身未同步（其 `chronyc tracking` 显示 `Stratum=0`），先修控制节点——把计算节点指向一个控制节点没用的时间源，两端时钟仍对不齐。

2. 在计算节点配置 chrony，配置文件路径和服务名因发行版而异：

   ```bash
   # ── RPM 系（OpenCloudOS / TencentOS / RHEL / CentOS）：/etc/chrony.conf，服务名 chronyd ──
   sudo sed -i '/^server /d; /^pool /d' /etc/chrony.conf
   echo 'pool pool.ntp.org iburst' | sudo tee -a /etc/chrony.conf
   # 或用内网 NTP server：
   # echo 'server <your-ntp-server> iburst' | sudo tee -a /etc/chrony.conf
   sudo systemctl restart chronyd

   # ── Debian / Ubuntu：/etc/chrony/chrony.conf，服务名 chrony ──
   #（Ubuntu 默认用 systemd-timesyncd，若未装 chrony 需先安装）
   # sudo apt-get install -y chrony
   sudo sed -i '/^server /d; /^pool /d' /etc/chrony/chrony.conf
   echo 'pool pool.ntp.org iburst' | sudo tee -a /etc/chrony/chrony.conf
   # 或用内网 NTP server：
   # echo 'server <your-ntp-server> iburst' | sudo tee -a /etc/chrony/chrony.conf
   sudo systemctl restart chrony
   ```

   > 解析到多个地址的名字（如 `pool.ntp.org`）用 `pool` 指令；`server` 会在启动时只固定一个解析到的地址。单个内网 NTP 主机用 `server`。

3. 重启 chrony 并确认 offset 收敛到接近 0（RPM 系：`chronyd`；Debian/Ubuntu：`chrony`）。重启后若偏移很大，chrony 默认用**slewing**（缓慢纠正，默认最大速率 ~8.3%/s）而非步进——超过 15s 的偏移要远多于一个心跳周期才能补上，几分钟/几小时的偏移可能要几小时。若需快速恢复，立即步进时钟：

   ```bash
   sudo systemctl restart chronyd    # RPM 系 —— 或：sudo systemctl restart chrony   （Debian/Ubuntu）
   chronyc tracking   # Stratum 应为非 0
   sudo chronyc makestep   # 立即步进时钟，不走 slewing；offset 仍大可重复执行
   chronyc tracking   # offset 此时应接近 0
   #（或在 chrony 配置加 `makestep 1.0 3`，让前 3 次超过 1s 的更新自动步进）
   ```

4. 时钟对齐后，节点在下一次心跳被评估时变为健康，验证：

   ```bash
   cubeopscli node ls   # 修复的节点 Healthy=True
   ```

无需重启 `cubelet`/`cube-ops`、无需改配置——两端时钟对齐后，现有心跳流即被判为新鲜。

## 参考

- 心跳判定：`CubeOps/internal/nodemanagement/service/health.go`（`EvaluateHealth`、`MetadataTimeout`）
- 心跳上报：`Cubelet/pkg/cubelet/node_status.go`（`HeartbeatTime: kl.clock.Now()`）
- 相关：#455（stale node health 处理——本问题是另一种失败模式：心跳按时到达，但两端时钟不一致）
