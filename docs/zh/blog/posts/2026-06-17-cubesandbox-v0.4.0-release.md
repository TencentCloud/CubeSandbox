---
title: "Cube Sandbox v0.4.0：从隔离 Agent 到治理 Agent"
date: 2026-06-17
author: Cube Sandbox 团队
description: "继 v0.3.0 快照/克隆/回滚三件套之后，v0.4.0 由 15 位贡献者合入 58 个 commits，核心解决三个问题：出站治理（CubeEgress L7 代理 + 凭据注入 + 域名过滤 + 访问审计）、可观测性（容器日志转发 + vsock 通道）、一致性（节点组件版本矩阵 + 模板兼容性对账）。同时带来网络 P99 延迟降 41%、模板构建峰值磁盘从 4.2 倍降到 1.2 倍。"
featured: true
weight: 1
---

# Cube Sandbox v0.4.0：从"隔离 Agent"到"治理 Agent"

继两周前 v0.3.0 发布了快照、克隆、回滚"三件套"后，本周 v0.4.0 正式上线。这一版的更新清单看起来庞杂，但如果只用一句话概括，就是：**Cube 正在从一个"高性能隔离运行时"，长成一个"可治理的 Agent 基础设施"**。

隔离做到了极致之后，下一个问题不仅仅是"怎么把 Agent 关起来"，而是演变成：

- **出站治理**——Agent 在沙箱里能往外打什么流量？
- **可观测**——沙箱里发生的事，外面看得见吗？
- **一致性**——集群里几十台机器，跑的到底是不是同一个版本？

本次 v0.4.0 核心解决的就是这三个问题。

## 一、域名过滤

Cube 的网络模型从第一个版本起即支持 AllowOut / DenyOut 的 IP/CIDR 过滤，可以按 IP 粒度对沙箱出站流量做精细控制，但真实业务场景的出站访问往往以域名的方式，通过 DNS 解析获得 IP 地址，配置策略时还不确定域名对应的 IP 地址，并且域名对应的 IP 随时可能发生变化。这套模型在原型阶段够用，但在生产环境下就开始捉襟见肘。

基于域名的过滤增强了出站流量管控，更贴合真实 Agent 使用沙箱的方式。在 AllowOut 中配置 1 个或多个域名，意味着放通该域名对应 IP 的流量；在 AllowOut 中可以同时配置域名和 IP/CIDR。域名不仅支持精确匹配，还支持后缀通配符方式，如 `*.qq.com` 即放通任何以 `.qq.com` 为后缀的子域名。DenyOut 不支持域名，当 AllowOut 中有至少 1 个域名时，DenyOut 必须包含 `0.0.0.0/0`，以拒绝任何未放通域名的流量。

## 二、CubeEgress：沙箱第一次有了"L7 出境管控"

不管是 Cube 第一个版本起即支持的 AllowOut / DenyOut 的 IP/CIDR 过滤，还是域名过滤，都是以 IP 地址粒度对沙箱出站做精细控制，类似业内常见的做法——用 NetworkPolicy 做 L3/L4 隔离。但 Agent 的世界已经不在 L3/L4 这个抽象层了，Agent 调用的是 `https://api.openai.com/v1/chat/completions`，是 host + path + method 的组合。NetworkPolicy 看不到这一层。

CubeEgress 是一个全新的基于 OpenResty 的出站网关，通过 TPROXY 截获沙箱出站流量，在请求离开集群之前执行 L7 策略。它由运行在 OpenResty/nginx 上的 9 个 Lua 模块（约 2200 行代码）以及 Go 端的集成组成——CubeAPI（提供 API 配置沙箱出站策略）、CubeMaster（生成并烘焙 CA 证书到模版）、NetworkAgent（策略下发）和 CubeEgress（策略执行）。在 rules 策略中配置的 SNI 和 host 会自动在 AllowOut 中放通域名，且该域名对应 80/443 的 HTTP/HTTPS 流量将从 CubeVS 重定向到 CubeEgress 处理。

**因此，CubeEgress 的设计就是把管控点放在 L7。**它的工作方式，是通过 TPROXY 透明按需截获沙箱的出站流量——沙箱内的代码无需任何感知，请求像往常一样发出去，但在离开集群之前必须先过它这一关。

**策略匹配维度是五个：SNI、host、method、path、scheme。动作是四个：allow、deny、audit、inject，**同时也支持写 Lua 脚本轻松扩展任何需要的功能。

### 2.1 L7 应用层过滤

IP/CIDR/域名的过滤，都是针对目的 IP 粒度的过滤。CubeEgress 支持 SNI、host、method、path、scheme 五元组的过滤，SNI、host 都支持后缀通配符，path 支持前缀通配符，每个元素也可以不配置，但 SNI/host 至少配置其一，以将流量引流到 CubeEgress。

每个沙箱可以配置若干 rule，每个 rule 可以配置五元组过滤，顺序匹配，每个规则可以 allow 或 deny 以实现精细的过滤策略。

### 2.2 HTTPS 代理

CubeEgress 处理沙箱发出的 HTTPS 流量时需要对流量解密。即 CubeEgress 支持自分发证书服务，沙箱进行 HTTPS 访问时接收到 CubeEgress 分发的证书，并与 CubeEgress 完成加解密处理后，CubeEgress 再与原目的服务进行 HTTPS 交互。

制作模板时 Cube 自动将 CubeEgress 的根证书烘焙到沙箱镜像中，以使沙箱证书验证成功，这一切都在 Cube 内部闭环完成。

### 2.3 关于凭据注入：Agent 代码再也不必接触你的 API Key

传统做法里，沙箱里的 Agent 要调 OpenAI，`OPENAI_API_KEY` 通常以环境变量的形式注入沙箱。这意味着：Agent 的 Python 代码读得到、它 import 的任何第三方库读得到、它跑的 LLM 上下文里可能出现、它打的日志里可能落盘。任何一环失守，Key 就出去了。

CubeEgress 的做法是反过来——**Key 只存在于代理层，永远不进沙箱**。

你在 `EgressRule.inject` 里配置好"凡是发往 `api.openai.com` 的请求，自动追加 `Authorization: Bearer ${KEY}`"，沙箱内的 Agent 代码只管发裸请求或携带任意 KEY，真实 Key 都由 CubeEgress 代理在转发时附加到 Header。从代码到日志，Agent 自始至终接触不到原始 Key。在 CubeEgress 自己的安全审计日志里，凭据字段也会被替换为 `***REDACTED***`，避免审计日志反过来成为泄密通道。

### 2.4 关于审计：每一次出站，都留下可追溯的结构化记录

每个经过 CubeEgress 的请求，都会生成一条结构化 JSON 日志——去了哪个域名、用了什么方法、Header 里有什么、Body 里有什么。配合 redactor Lua 模块做请求体脱敏，你既能事后追溯 Agent 访问了什么，又不会把用户隐私写进日志。

## 三、容器日志转发：沙箱内的输出，第一次能被宿主机"看见"

之前的沙箱存在一个问题：容器进程 stdout/stderr 的输出留在沙箱内部，宿主机上看不到。调试要 `cubecli exec` 进去翻，生产环境的日志聚合根本无从下手。

这件事的难点在于：沙箱是硬件隔离的 MicroVM，不是普通容器，stdout/stderr 不能像 `docker logs` 那样直接读宿主机进程的 fd。你必须在沙箱内部捕获、跨虚拟机边界传输、宿主机侧落盘——而且不能借用业务网络，否则日志和应用流量混在一起，又快又乱。

v0.4.0 的做法是开了一条**专用 vsock 通道**：

- **沙箱 agent 侧**：通过 shim 注入的 `cube.container.log_forwarding=true` 注解感知到要开日志通道，为 init 进程的 stdout/stderr 创建管道，1 MiB 缓冲、`O_NONBLOCK` 非阻塞——这两个参数是关键，**保证日志写慢了不会反过来阻塞 Agent 进程**。
- **传输**：日志走专用 vsock 连接（与控制面、业务网络物理隔离），shim 在宿主机侧消费。
- **查看**：沙箱日志查看：`cubecli logs ${sandboxid}`，模版日志查看：`cubecli logs --tpl ${templateid}`。
- **退出时序**：进程退出时主动关闭写端 FD 确保读端收到 EOF。这是个看似琐碎但极易踩坑的细节——少了它，最后几行日志可能永远收不到。

配套新增了 `cubecli cubebox logs` 子命令，支持 `--tail`/`--head`/`--all`/`--stderr`。

## 四、节点组件版本矩阵：让"版本漂移"无处藏身

你升级了 CubeMaster，忘了某台节点的 Cubelet；你刷新了 guest-image，有两个节点 kernel 没跟上。这种不一致不会立刻爆炸，但会在某一天以一种你完全意想不到的方式出问题——某个沙箱在 A 节点能跑、在 B 节点起不来，排查到最后才发现是某个组件版本差了两个小版本。

Kubernetes 用了多年才在这件事上形成共识：**版本可见性是集群运维的零号问题**。v0.4.0 把这一层基础设施搭起来了，分为了三步：

### 4.1 第一步：编译期统一注入版本元数据

所有 Go 二进制通过 ldflags、所有 Rust 二进制通过 build.rs，在编译期注入 version / commit / build-time 三元组。一键部署脚本生成 `release-manifest.json`。`cubecli version` 和 `cubemastercli version` 的输出格式完全一致。

听起来简单，但做过多语言异构项目的人都知道——**让 Go/Rust 两套构建链产生格式一致、来源一致的版本信息，是一件需要刻意约束才能做到的事**。在此之前，"同一个组件，不同工具查出来版本不一样"是 Cube 也存在过的现实。

### 4.2 第二步：节点自动上报、CubeMaster 维护矩阵

Cubelet 在节点侧自动采集 guest-image、cube-agent、kernel 及控制面组件的版本，上报 CubeMaster。CubeMaster 在 `node_component_version` 表（迁移 0004）维护全集群版本矩阵，按组件分组报告同版本节点，**版本偏差以集合差的形式暴露**——哪台节点和大部队不一样，一眼就能看到。

CubeAPI 同时提供了汇总和详情接口，Web UI 新增 `Versions.tsx` 页面做可视化呈现。

### 4.3 第三步：模板和组件版本"对账"

这一步比前两步更进一步。Cube 引入了 `template_versions` 表（迁移 0006）来记录每个模板副本依赖的组件版本，并提供两个端点：

- `/templates/compat`：全集群模板兼容性视图
- `/templates/compat/{id}`：单模板详情

Web UI 配套引入了 CompatBadge / CompatSection / CompatWarning / CompatNodeCard / VersionDeltaList 五个组件——过时的副本、版本差异、需要重建的横幅，全部可视化。还支持"版本绑定"，把模板**固定到特定组件版本**，避免节点升级后模板悄无声息地坏掉。

## 五、那些不在 Highlight 里、但同样重要的事

### 5.1 网络性能：P99 延迟降 41%

GetTapFile 这条沙箱启动关键路径被重写成三层策略：缓存命中走快速路径（0 系统调用）、池化 fd 已关闭走热路径（2 系统调用，跳过昂贵的 restoreTap）、无状态走完整恢复路径。fdserver 的 JSON 响应携带 ifindex，让 cubelet 直接跳过 netlink.LinkByName——这是个常被忽略但成本极高的并发序列化点。同时把 EnsureNetwork/ReleaseNetwork 之间的 TOCTOU 竞态，用 per-sandbox creating 守护通道替代了 singleflight。

BMI5 环境实测：

- 网络 P50：35.3 → 23.1ms（**-35%**）
- 网络 P99：86.6 → 51.2ms（**-41%**）
- 整体启动 P50：106.1 → 92.0ms（**-13%**）
- 吞吐：194.8 → 209.8 sandboxes/s

### 5.2 模板构建：峰值磁盘从 4.2 倍降到 1.2 倍

模板镜像构建管线做了一次彻底重构——引入 skopeo + umoci 走**无守护进程路径**。

其中，Cube 在 5 个细节上做了磁盘优化：包括 Docker export stdout **通过 1 MiB 管道直连 `tar -xf stdin`**，消除中间 rootfs.tar；ext4 空间估算用**三重开销精确模型**（256 MiB 固定 + 数据 10% + 每文件 1 KiB），按 256 MiB 边界对齐等。

最终优化结果是使**峰值磁盘使用从约 4.2 倍镜像大小降到约 1.2 倍**。文件级 SHA256 指纹还实现了跨构建去重。SDK 侧的 `POST /templates` 同步支持了 DNS、出口 CIDR、镜像仓库认证、command/args、网络类型和节点范围等更完整的选项。

此外，**在调度层面，实现了可配置超卖、自定义节点亲和性。同时，也修复了一系列核心 Bug**，例如：禁用互联网时不再自动放行 DNS（之前是 deny-all 策略下的隐蔽漏洞，影响安全语义一致性）、DNS 服务器 IP 自动加入 AllowOut（之前需要用户手动配置，新手必踩）等。

## Coming soon...

v0.4.0 把"出站治理"和"可观测性"两块拼图补上了，但我们还会继续在下一个版本里将这些功能推送给大家：

- **ARM 物理机支持**：目前 Cube 还是一个 x86-first 的运行时，但 AI 基础设施侧的现实早已是双架构并行，越来越多团队希望"把 Agent 跑在 ARM 上"。下一版 Cube 计划将 MicroVM 内核、guest-image、cube-agent 全栈打通 ARM。

- **Volume SDK**：v0.3.0 的快照/克隆/回滚解决了"沙箱状态"的管理，但用户数据和沙箱生命周期仍然是绑死的。接下来，Volume SDK 会允许独立创建、可在沙箱之间挂载迁移、可独立做快照和备份。这意味着 Agent 从此可以拥有一个跨会话、跨实例的"工作空间"，而不只是一个一次性运行环境。

- **自动暂停与恢复（autopause）**：闲置沙箱占着内存和 CPU 配额是一笔实实在在的成本——Agent 等用户输入、等长任务回调、等下一轮 RL rollout 的间隙，沙箱大部分时间其实是"空跑"。autopause 会让沙箱在空闲时完整释放宿主机内存与 CPU 配额、只保留磁盘状态，被访问时再毫秒级唤醒。这个功能叠加 v0.3.0 的快照机制后，Cube 能把"暂停"做到接近 0 内存占用、"恢复"做到亚百毫秒级。

当前，Cube 已经走过了"造出一个高性能 MicroVM 沙箱"的阶段。从 v0.4.0 开始，它要回答的问题是：**当 AI Agent 真的大规模跑在生产环境里，沙箱该提供什么样的治理能力？**

如果你正在构建 Agent 工作流、Agentic RL 训练平台或者面向开发者的代码执行服务，欢迎来 GitHub 拍砖、提 issue、发 PR。

**GitHub 仓库：** https://github.com/TencentCloud/CubeSandbox

**v0.4.0 完整 Release Note：** https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/changelog/v0.4.0.md
