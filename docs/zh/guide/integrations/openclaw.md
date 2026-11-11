---
title: "OpenClaw 集成指南（多租户编码智能体）"
author: yjjnls
date: 2026-11-10
tags:
  - integration
  - openclaw
  - coding-agent
  - network-policy
lang: zh-CN
---

# OpenClaw 集成指南（多租户编码智能体）

## 部署背景

一个平台工程团队为内部开发者提供 AI 编码智能体（OpenClaw）服务。这些智能体会审查 diff、编辑工作区文件、执行构建与测试、调用模型 API——运行的是事先无法完全人工审查的 shell 命令和生成代码。平台需要为每个智能体会话提供硬隔离边界、严格的出网管控、跨维护窗口的会话连续性，以及快到开发者无感知的冷启动。

[OpenClaw 集成示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration)已经介绍了如何通过 E2B 兼容 SDK 把 `cube-sandbox` skill 接到单个沙箱上。本指南讲的是下一步：把这套方案作为多租户共享服务运行，并满足真实的安全与生命周期要求。

## 关键挑战

- **隔离边界。** 智能体执行未经审查的命令和模型生成的代码。共享内核的容器不被安全团队接受为边界；每个会话需要独立的内核。
- **出网管控。** 智能体需要的出网目标其实很少：模型 API、包仓库、内部 git 镜像。要求是默认拒绝（deny-by-default）——而不是只审计不拦截——且白名单要像代码一样走评审。
- **会话连续性。** 编码会话动辄运行数小时到数天，必须扛住节点维护，不能丢网关状态和工作区。
- **交互延迟。** 开发者是对着一个活着的智能体打字；以秒计（而非毫秒计）的冷启动会把工作流退化成轮询。
- **入站访问。** 每个沙箱内的 OpenClaw 网关要暴露 HTTP UI/API 给开发者的浏览器和 CLI——同时不能给沙箱网络开任何其他入站通路。

## 基于 Cube Sandbox 的方案

每个智能体会话对应一个从预烘焙模板创建的 Cube 沙箱：

```
开发者浏览器 / CLI
        │  （映射的网关端口）
        ▼
OpenClaw 网关  ◄─── OpenClaw 智能体（skills、CLI 工具）
        │  E2B 兼容 SDK
        ▼
CubeAPI ──► CubeMaster ──► Cubelet ──► MicroVM（每会话一台，独立内核）
                                          ├─ 工作区（持久卷 / host mount）
                                          └─ 出网：默认拒绝 + 白名单（eBPF）
```

**用模板，而不是现场装环境。** OpenClaw、它的 skills、以及智能体实际会调用的工具链（git、node、构建工具）都烘焙进一个 Cube 模板。每个会话都从已知良好的镜像启动，开发者看着屏幕时没有任何安装动作。基于模板的创建把冷启动压在亚秒级，与项目自己的[性能基准](/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark)一致。

**出网策略即代码。** 创建会话时关闭公网出网，只保留显式白名单。域名目标由 Cube 的 DNS 查询/应答学习机制处理，所以服务方更换 IP 后白名单条目依然有效：

```python
sandbox = Sandbox.create(
    template="openclaw-agent",
    allow_internet_access=False,
    network={
        "allow_out": [
            "api.llm-provider.example",   # 模型 API（域名，经 DNS 跟踪学习）
            "registry.npmjs.org",         # 包仓库
            "pypi.org",
            "10.60.0.0/16",               # 内部 git 镜像（CIDR）
        ],
    },
)
```

Cube 会安装 `0.0.0.0/0` 拒绝规则，并在 eBPF 数据面执行"允许优先于拒绝"。沙箱私网与宿主内部网段（`10.0.0.0/8`、`127.0.0.0/8`、`169.254.0.0/16`、`172.16.0.0/12`、`192.168.0.0/16`）默认在拒绝集合里，普通公网出网触不到任何内部服务——但显式的 `allow_out` 条目优先于拒绝，上面 `10.60.0.0/16` 的内部 git 镜像正是靠这一点打通的。凡是没有进白名单的目标一律不可达，这直接解决了安全评审里最常见的"智能体可达集群内部服务"问题。策略变更走与其他平台组件一致的评审配置，而非临时手工修改——参见[网络策略](/zh/guide/network-policy)。

**入站走端口映射。** 网关端口在创建沙箱时映射，并通过平台 URL 暴露。端口映射是这套部署依赖的主要入站路径，但不是硬性的网络边界：CubeProxy 可按 `<port>-<sandbox-id>.<domain>` 路由沙箱的任意端口，且沙箱 URL 默认公网可达，除非显式限制公网流量（`network.allow_public_traffic=False`）。出网策略不约束入站，所以入站侧也单独过了一遍评审。

**会话连续性。** 空闲会话先暂停以释放 CPU/内存、保留状态；开发者回来时一次 connect 即可恢复：

```python
sandbox.pause()                       # 做检查点，释放 CPU/内存
# ……稍后
sandbox = Sandbox.connect(sandbox.sandbox_id, timeout=300)
```

维护窗口场景下，工作区放在持久存储上（host mount 或 S3 volume 插件——见[持久化存储](/zh/guide/persistent-storage)），长时间运行的任务用快照做检查点，节点排空后会话可以原地恢复——见[快照/回滚/克隆](/zh/guide/snapshot-rollback-clone)。跨节点恢复还要求共享快照后端（`jfs` 或 `s3`，且 `remote_status=ready`）；节点本地的 `xfs` 存储扛不住排空。

## 运行效果与取舍

- 每会话一台 MicroVM 的边界通过了共享内核方案没能通过的安全评审：智能体代码与宿主基础设施之间隔的是内核边界，不只是命名空间。
- 默认拒绝的出网管控在日常运行中站住了。出现的出网失败都是智能体撞上未列入白名单的镜像源，通过在白名单里补条目解决——从未靠放宽默认策略解决。
- 会话扛住了维护。暂停、持久化工作区、快照恢复三者配合，让跨越数天的智能体任务在节点排空后继续存活。
- 创建走模板，交互延迟保持低位；安全姿态没有牺牲工作流的响应速度。

照搬这套设计前值得了解的取舍：

- 每会话一台 MicroVM 的内存开销高于容器。把模板资源调准、空闲会话及时暂停，密度就可以接受。
- 域名白名单依赖会过期的学习型 A 记录。长会话闲置超过老化窗口后，下一次请求会重新解析，通常无感；但建议对"已知良好域名出现拒绝尖峰"做告警，而不是默认每个拒绝都是攻击。
- 快照存储随会话数增长；要按计划清理冷快照。

## 参考

- OpenClaw 集成示例（skill 接线）：[examples/openclaw-integration](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration)
- 网络策略（allow/deny、DNS 学习、条目上限）：[网络策略](/zh/guide/network-policy)
- 快照、回滚与克隆：[快照/回滚/克隆](/zh/guide/snapshot-rollback-clone)
- 暂停/恢复生命周期语义：[生命周期](/zh/guide/lifecycle)
- E2B 兼容性：[E2B 兼容性](/zh/guide/e2b-compatibility)
