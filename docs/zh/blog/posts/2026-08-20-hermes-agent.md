---
title: "在 Cube Sandbox 中运行 Hermes Agent：从持久化挂载、Skills 分层到网络与恢复故障的工程实践"
date: 2026-08-20
author: 陈金博（上海阳璞新能源科技 AI 团队）
description: "一个常驻 Agent 应用需要沙箱记住的不只是任务产物，还包括 Agent 自身的配置、历史会话、以及一批被反复调用的 Skills。而沙箱本身天然是易失的——重启、暂停、迁移都可能让这些状态归零。本文分享上海阳璞新能源科技 AI 团队在将 Hermes Studio 迁上 Cube Sandbox 的过程中踩过的坑和落地方案。"
featured: false
---

# 在 Cube Sandbox 中运行 Hermes Agent：从持久化挂载、Skills 分层到网络与恢复故障的工程实践

作者｜上海阳璞新能源科技 AI 团队 · 陈金博

**编者按｜** 对多数团队而言，一个常驻 Agent 应用需要沙箱记住的不只是任务产物，还包括 Agent 自身的配置、历史会话、以及一批被反复调用的 Skills。而沙箱本身天然是易失的——重启、暂停、迁移都可能让这些状态归零。于是，不同的工程团队会撞上几类相似的问题，例如：挂载目录的路径、共用 Skills 的统一维护、内网模型服务连通等等。

上海阳璞新能源科技 AI 团队在将 Hermes Studio（内部 Agent 运行平台）迁上 Cube Sandbox 的过程中，踩过一些坑，并基于此建构了一套完整的落地方案。其核心思路是以 Cubelet 节点上的持久化目录作为唯一存储底座，通过 `metadata["host-mount"]` 将 Hermes home、workspace 和公共 Skills 分别挂载进沙箱；管理面板负责目录初始化、公共 Skills 部署、沙箱创建、服务启动与状态查询。本文将对该实践进行分享。

## 一、架构与运行链路

整套方案以 Cube 官方 sandbox-code 基础镜像构建 Hermes Studio 全运行时模板：保留 Cube command service（端口 49983），并提供 Hermes Studio Web UI（端口 9000）。模板内含 Hermes Agent 运行环境、Node 和 hermes-web-ui，新建沙箱直接具备可用的 Agent 运行能力。

管理面是用 FastAPI 和服务端渲染实现的单管理员内部面板，通过 Cube 官方 Python SDK 调用管理沙箱，支持创建、连接、查看、暂停、恢复、停止，并展示 Web UI 访问地址与健康检查结果。

![Hermes Studio 基于 Cube Sandbox 的部署、持久化与访问架构](./assets/2026-08-20-hermes-agent/01-hermes-architecture.jpg)

*图 1：Hermes Studio 基于 Cube Sandbox 的部署、持久化与访问架构*

从管理员提交创建请求，到 Hermes Studio 对外可访问，完整流程可归纳为十步：

1. 管理员通过本地运行的 Hermes Sandbox Panel 创建或管理沙箱。面板是单管理员内部工具，负责表单交互、配置加载、Cube SDK 调用，以及状态与访问地址展示。
2. 创建请求指定三个核心要素：home-id 对应一个持久化 Hermes home；workspace-id 对应一个持久化项目工作区；Template 使用已验证的 Hermes Studio Full Runtime 模板。
3. 面板先通过 SSH 登录 Cubelet 节点，确保 `/data/shared/hermes-homes/<home-id>`、`/data/shared/hermes-workspaces/<workspace-id>` 和 `/data/shared/hermes-common-skills` 三个 host-mount 目录存在。
4. 对于首次使用的 home-id，面板检查目录中是否存在 `config.yaml`。若不存在，则从 `/data/shared/hermes-homes/default` 复制默认配置与运行内容；已初始化的 home 不会被覆盖。
5. 面板调用 Cube SDK 创建沙箱，通过 `metadata["host-mount"]` 配置三类挂载：Hermes home 映射到 `/root/.hermes`（读写）；项目工作区映射到 `/workspace`（读写）；公共 Skills 映射到 `/opt/hermes-common-skills`（只读）。
6. 新沙箱启动后，面板先初始化公共 Skills 覆盖层，再启动 Hermes Studio。仓库 `skills/` 通过 rsync 部署至 Cubelet 公共目录；沙箱内为尚不存在的公共 Skill 创建符号链接，同名私有 Skill 优先保留。
7. 面板启动 hermes-web-ui，读取 Web UI token，组合出可访问的 Hermes Studio URL。模板对外暴露 9000（Web UI）和 49983（Cube command service）两个关键端口。
8. 面板检查 Hermes Web UI 与 `49983/health`。健康检查失败时不自动销毁沙箱，以便管理员继续连接诊断，或执行暂停、恢复与停止。
9. 后续用同一 home-id、workspace-id 创建新沙箱时，会重新挂载同一份 Cubelet 目录，Hermes 配置、会话、私有 Skills 与工作区文件均可跨实例保留。公共 Skill 内容更新会被已挂载沙箱直接看到；新增 Skill 目录则需执行一次"刷新公共 Skills"以创建新符号链接。
10. 启动 Hermes Studio 时，面板将默认环境变量与创建时覆盖项合并导出；页面展示与错误输出按 KEY、TOKEN、SECRET、PASSWORD、API_KEY 等规则脱敏，避免敏感值暴露在管理界面。

该项目目前处于内测阶段：在单个 Cube Sandbox 节点上接入数十个 Hermes 沙箱，已持续运行约一个月。期间出现过若干工程问题，但均已完成定位并形成处理方案，具体如下。

## 二、持久化设计，切换 host-mount 方案时踩过的坑

沙箱重建后，Hermes Agent 的运行状态需要能恢复。最需要持久化的是 `/root/.hermes`（配置、运行文件、会话与私有 Skills）和 `/workspace`（项目文件与任务工作区）。

按 E2B 风格 SDK 的常规用法，我们团队最初尝试通过 `Sandbox.create()` 的 `volume_mounts` 参数挂载持久化目录，但当时的 Cube 版本（v0.6.0 之前）尚不支持 `volume_mounts`。于是在官方建议下，改用 host-mount 方案。

我们的验证路径是逐步收窄的：先确认沙箱创建请求支持透传 metadata；再验证 `metadata["host-mount"]` 可携带多个挂载描述（hostPath、mountPath、readOnly）；接着做最小实验——第一个沙箱向挂载后的 `/root/.hermes` 写入 marker 文件，第二个新沙箱挂载同一 host 目录并成功读取，同时确认挂载类型为 virtiofs 读写挂载；最后把最小方案扩展为三类挂载：持久化 home、持久化 workspace、只读共享公共 Skills。

这个过程中踩过七个坑，其中前四个是关于挂载本身的：

1. **hostPath 不是管理面板所在机器的路径。** 它必须指向实际承载沙箱的 Cubelet 节点。误解为 FastAPI 面板进程所在主机的路径，会导致挂载找不到预期数据。
2. **目标目录必须预先存在。** 即使挂载描述正确，只要 Cubelet 节点上缺少目标目录，CubeMaster 就会在创建阶段报错。

因此当前流程会先通过 SSH 创建 home、workspace 与公共 Skills 目录，再调用 Cube SDK。

3. **空目录会覆盖镜像内的 `/root/.hermes`。** 挂载不是目录合并——空 host 目录会遮蔽镜像预置的 `config.yaml`、Agent 运行文件与 Skills，是最关键的运行时问题。
4. **必须先 seed，再正式挂载。** 早期用临时 seed sandbox 把镜像中的 `/root/.hermes` 复制到 host 目录；当前则在 Cubelet 固化 default home，以 `config.yaml` 是否存在判断是否需要初始化，避免反复覆盖用户会话与私有配置。
5. **`/root/.hermes` 与 `/workspace` 不应共用目录。** Hermes home 属于 Agent 运行态与用户配置，workspace 属于项目文件，分离两者能降低会话、Skills 与业务代码之间的耦合。
6. **公共 Skills 不能直接复制到每个 home。** 逐份复制会造成副本膨胀、更新不同步，还可能覆盖私有修改——这直接引出了下一节的分层设计。
7. **旧沙箱无法动态补充新 mount。** 公共 Skills 是后续新增的第三类挂载，创建时未包含该 mount 的旧沙箱无法仅靠"刷新 Skills"补上，必须重建并在创建参数中加入挂载。

## 三、Skills 分层：只读公共目录 + 符号链接覆盖

这一设计要同时满足两个互斥的诉求：公共 Skills 要能被多个 Cube 沙箱统一使用和更新；每个 Hermes home 里已有的个人或项目定制 Skills 又不能被破坏。

直接复制会遇到四个问题：每个 home 保留一份副本，公共 Skill 更新后要逐一同步；难以区分公共版本与用户自行修改的私有版本；同步时容易覆盖私有修改；删除或升级公共 Skill 时可能误删 home 中的私有文件。

最终的数据链路是这样的：

公共目录只维护一份，可通过 Git 审计与版本管理；以只读方式挂入沙箱后，单个 Agent 或用户无法误改全局能力。由于 Hermes Agent 默认从 `/root/.hermes/skills` 加载 Skills，面板会在该目录中创建指向 `/opt/hermes-common-skills/<skill-name>` 的符号链接。

| 内容 | Cubelet 目录 | 沙箱路径 | 权限 |
|---|---|---|---|
| Hermes home | `/data/shared/hermes-homes/<home-id>` | `/root/.hermes` | 读写 |
| Workspace | `/data/shared/hermes-workspaces/<workspace-id>` | `/workspace` | 读写 |
| 公共 Skills | `/data/shared/hermes-common-skills` | `/opt/hermes-common-skills` | 只读 |

覆盖规则很简单：如果 `/root/.hermes/skills/<name>` 已存在，视为私有 Skill，不创建同名公共符号链接，私有版本优先。之所以不把公共目录直接挂载到 `/root/.hermes/skills`，是因为挂载会遮蔽 home 中已有的私有 Skills。

共享 host-mount 使已挂载沙箱能直接看到公共 Skill 的内容更新；如果新增了一个 Skill 目录，仍需执行一次刷新，为该目录建立新的符号链接。

## 四、网络放行与镜像构建

### 4.1 局域网模型访问失败：出站规则需单独放行

运行过程中，我们遇到一个问题：Hermes Studio 与 Agent 均能在 Cube 沙箱中正常启动，Web UI 也能打开——但一旦发起模型调用，就会出现 Connection error。排查后，发现原因是：模型服务部署在 `10.10.x.x` 等局域网地址时，`--allow-internet-access` 只管出公网，不代表沙箱能访问内网服务，两者是两条完全不同的出站规则。

我们当时先在沙箱做 TCP 检查，确认是网络层没打通而不是应用层问题：

随后重新创建 Template，显式增加 IPv4 出站 CIDR：

`--allow-out-cidr 0.0.0.0/0` 才是真正放开沙箱向 IPv4 地址出站访问的开关，覆盖了当前局域网模型服务地址。放行后沙箱到模型服务的 TCP 连接恢复可达，请求 `/v1/models` 返回 401 Invalid token——网络链路已经打通，之前的 Connection error 消失，只是测试请求没带模型 API token。

### 4.2 AppleDouble 文件：在构建 Cube 镜像前清理 macOS 元数据

Hermes Agent 源码需要进入 Cube Template 对应的镜像。我们团队的构建链路为：macOS 开发机 → 打包源码 → 上传至 Cube 构建机 → Docker 构建模板镜像 → Cube 沙箱运行 Agent。

最初从 macOS 打包时，源码中混入了 `._*` 等 AppleDouble 元数据文件。它们不会阻止镜像构建或沙箱启动，但 Hermes Agent 扫描源码时可能把它们当 UTF-8 文本读取，触发解码错误。

最终处理点不在沙箱运行逻辑，而在 Cube 镜像构建上下文的制作阶段：

`COPYFILE_DISABLE=1` 尽量减少 macOS 生成 AppleDouble 文件；两个 `--exclude` 确保即使目录里已经有这些文件，也不会打进上传包。镜像构建完成后可在沙箱内验证，预期结果为 0。

## 五、v0.5.1 中，暂停恢复时的 TAP 设备 EBUSY 问题

我们在 Hermes sandbox 配置了 15 分钟无访问自动暂停，以节省资源。部分实例进入暂停状态后，控制面重启了 CubeMaster、Cubelet 与 network-agent。但此后再访问这些实例时，会出现恢复失败，CubeMaster 报错 Device or resource busy。

初步定位为：暂停快照中记录的虚拟网卡，与控制面重启后 network-agent 保留或初始化的 persistent TAP 网卡状态未能正确协调。恢复时系统重复配置同一 TAP 设备，最终触发 EBUSY。新建且持续运行的沙箱不受影响；暂停后经历控制面重启的实例则可能无法访问。

目前，我们团队已向 CubeSandbox 官方提交 Issue [#953](https://github.com/TencentCloud/CubeSandbox/issues/953)，官方反馈正在修复。

因此，临时处理方案是：保留 home 与 workspace 持久化目录，销毁并重建无法恢复的沙箱，再重新绑定 alias。这样用户工作区和 Hermes 配置不会丢失，但 VM 临时层会被重置。

## 六、关键结论

- **持久化边界必须先于模板运行验证**：`/root/.hermes` 与 `/workspace` 应分离，避免运行态、用户配置与业务代码耦合。
- **空 host 目录会遮蔽镜像中原有的 `/root/.hermes`**；首次使用的 home 必须先完成初始化（seed）。
- **hostPath 指向实际承载沙箱的 Cubelet 节点**，而不是管理面板所在主机；目标目录必须在创建沙箱前存在。
- **公共 Skills 采用"只读共享目录 + 私有目录符号链接"的覆盖层设计**，实现集中维护与私有版本优先。
- **仅启用互联网访问不等于可访问局域网模型服务**；模板需显式配置出站 CIDR，并在沙箱内进行 TCP/HTTP 验证。
- **macOS 制作镜像构建上下文时**，应排除 AppleDouble（`._*`）与 `.DS_Store`，避免 Agent 扫描源码时触发解码错误。
- **控制面状态以 Cube 实时查询结果为准**，避免面板自维护状态与真实沙箱状态发生漂移。

## 其他问答摘录

**Q：管理面板为什么以 Cube 查询结果为准，而不是自己维护一套状态？**

A：为了确保唯一事实来源。若面板自行维护沙箱状态，长期运行后很容易与 Cube 的真实状态发生漂移。面板只展示 Cube 的实时查询结果，可以减少状态不一致与误操作。

**Q：如果重新设计一次，最想换掉什么做法？**

A：会从项目第一天就明确"持久化状态的边界"，而不是先验证 Hermes Studio 模板能运行，再补做 host-mount 持久化。持久化设计会直接影响目录初始化、Skills 分层、升级策略和故障恢复，越早验证，后续返工越少。

**Q：最希望分享给其他 Cube 用户的一条经验是什么？**

A：从第一天起就做一次完整的重建测试：创建一个全新沙箱，确认 Agent 能拿到正确的状态、能够访问所需网络，同时不会泄露或覆盖不应共享的数据。只有"重建后仍正确"，才说明持久化边界、网络规则和权限设计真正成立。

*感谢上海阳璞新能源科技陈金博老师及其 AI 团队，对以上内容的贡献。*
