---
title: "用 CubeSandbox 增强 OpenClaw 与 DSH：企业安全执行面实战"
date: 2026-09-30
author: runzhliu
description: "OpenClaw 和 DeepSeek Harness（DSH）不只是聊天页面，企业接入时要回答四个问题：执行是否安全、环境是否可复现、空闲资源能否回收、用户下一轮回来时是否足够顺滑。本文用一个部署在 Kubernetes 上的 CubeSandbox v0.7.0 集群，跑通 OpenClaw 和 DSH 通过 Skill 与 Tool Plugin / Cordis Plugin 创建 MicroVM、执行代码并自动销毁的四条真实链路。"
featured: false
---

# 用 CubeSandbox 增强 OpenClaw 与 DSH：企业安全执行面实战

作者｜runzhliu

> 本文首发于作者 aik8s 博客及公众号，经作者同意及授权，Cube Sandbox 公众号对原文进行转载，并做了少量适配性修改编辑。

OpenClaw 和 DeepSeek Harness（DSH）都不只是聊天页面。它们能读写文件、运行 Shell、调用浏览器、安装依赖并长时间保存会话。能力越接近真实开发机，企业越不能只问"能不能跑"，还要回答四个问题：执行是否安全、环境是否可复现、空闲资源能否回收、用户下一轮回来时是否足够顺滑。

带着这四个问题，我们用一个已经部署在 Kubernetes 上的 CubeSandbox v0.7.0 集群跑通四条真实链路：OpenClaw 2026.7.1 和 DSH 0.1.1-rc.2 分别通过 Skill，以及不读取 Skill 的 Tool Plugin / Cordis Plugin，创建 MicroVM、执行代码并自动销毁。这篇文章记录了验证的完整过程。

## 一、先确定边界：Agent 是控制面，Sandbox 是执行面

推荐架构如下：

![推荐架构：Agent 是控制面，Sandbox 是执行面](./assets/2026-09-30-openclaw-dsh/01-architecture.jpg)

*图 1：推荐架构——Agent 是控制面，Sandbox 是执行面*

这条边界很重要：

- OpenClaw / DSH 保留会话事实、用户身份、模型调用和审批记录；
- CubeSandbox 只拿到当前任务必需的工作区、资源、网络和短期身份；
- Sandbox 被攻破时，攻击者仍要跨过独立 MicroVM、出站策略和工具授权，才能影响其他会话或控制面；
- Sandbox 销毁不应删除 OpenClaw 状态目录或 DSH_HOME，两者属于不同生命周期。

如果把 Gateway、插件、模型密钥、浏览器 Profile 和不可信代码都放进同一个长期运行容器，MicroVM 的隔离价值会被大幅削弱。

## 二、它具体让 OpenClaw / DSH 好在哪里

### 2.1 更安全：从"限制目录"升级为"隔离执行世界"

DSH 自带的 dsh-sandbox 面向同一操作系统中的进程限制。官方说明它通过 bwrap、Landlock、Seatbelt 等机制控制文件副作用；容器、MicroVM 和远端执行并不是这个 seam 的 backend，而是要替换整组 Shell 与文件能力。

OpenClaw 启用 Sandbox 后，Gateway 仍留在宿主环境，工具执行可以进入 Docker、SSH 或 OpenShell backend。官方同样强调，这不是完美安全边界；插件和 Gateway 进程仍属于可信控制面。

CubeSandbox 增加的是另一层执行边界：

- 每个沙箱使用独立 MicroVM 内核；
- 每个会话拥有独立 rootfs、进程、网络和生命周期；
- 出站可以完全关闭，或按 CIDR、域名、协议、Host、Path、SNI 控制；
- 公开入口可以要求 per-sandbox traffic token；
- L7 代理可以只向允许的目标注入凭据，真实 Token 不必进入沙箱环境变量。

但 MicroVM 不能替代工具授权。模型是否可以调用 kubectl、删除仓库、提交代码或访问生产 API，仍要由 OpenClaw / DSH 和企业 Tool Gateway 决定。

### 2.2 更快：模板和快照消除重复准备

企业 Agent 最影响体验的往往不是模型首 Token，而是执行环境准备：拉镜像、安装 Node/Python、恢复工作区、启动浏览器、重新建立工具连接。

CubeSandbox 可以把这些动作前移：

- 把 Git、Python、Node、浏览器和企业 CA 固定进模板；
- 用模板 alias 管理环境版本，避免每个 Agent 自己安装；
- 空闲时 Pause，释放 CPU 和内存；
- 下一轮 Resume，恢复文件系统和 MicroVM 内存；
- 用 Snapshot、Rollback 和 Clone 支持并行尝试与快速回退。

本次 READY 模板的单次创建样本为 134 ms。Pause 与 Resume 是秒级，是否比重新创建更快取决于模板大小、快照后端、节点缓存和并发，因此要用真实工作区测试，不能直接套用这一个数字。

### 2.3 更顺滑：会话与沙箱生命周期一一映射

用户希望的是"过一会继续做"，而不是理解 Pod、容器和 VM。适配层应把底层状态隐藏起来：

控制面至少持久化这些字段：

| 字段 | 用途 |
|:--|:--|
| tenant_id / user_id / session_key | 确定租户与会话归属 |
| sandbox_id | 连接或回收 MicroVM |
| traffic_access_token | 访问受保护的数据面；加密保存 |
| template_alias / Digest | 保证环境可复现 |
| network_profile | 记录实际出站策略 |
| state / last_seen_at | Pause、Resume、Kill 与回收 |
| request_id / trace_id | 串联 Agent、API、代理和审计日志 |

所有生命周期操作都要幂等。创建成功但数据库写入失败、恢复超时、客户端断线和重复回调都不能留下无人认领的沙箱。

### 2.4 更适合企业平台：控制面和执行面可以分别扩展

OpenClaw / DSH Runtime 的压力来自模型流、会话、渠道和插件；CubeSandbox 的压力来自 VM 创建、CPU、内存、快照和网络。拆分后可以分别扩展、限流和升级：

- Runtime Pool 按业务、部门或信任域拆分；
- Sandbox Node Pool 按普通代码、浏览器、数据分析或 GPU 模板拆分；
- 企业控制面统一下发模板、网络、配额和 TTL；
- 一个 Runtime 不再需要本地 Docker Socket，也不需要把宿主目录直接交给 Agent。

## 三、OpenClaw 与 DSH 应该怎样接

### 3.1 OpenClaw：三条路径

| 路径 | 实现 | 优点 | 局限 | 建议 |
|:--|:--|:--|:--|:--|
| 官方 CubeSandbox Skill | Agent 按 Skill 指引调用 Cube SDK | 上手最快，已有官方示例 | 不是 OpenClaw 原生 backend；普通 exec 仍可能走其他执行面 | PoC |
| 企业 Adapter / Plugin Tool | 注册受审计的 cube_exec、cube_read、cube_write、cube_release | 参数和策略可控，容易加租户、审计和配额 | 需要维护少量适配代码；当前参考实现还没有 PTY | 推荐起点，本文已实测 |
| 整个 OpenClaw 运行在 CubeSandbox | 用 Digital Assistant / AgentHub 从 OpenClaw 模板创建助手 | 助手可快照、回滚、克隆 | 当前是 Preview；Runtime、状态和执行边界容易重新混在一起 | Demo、个人助手和早期验证 |

CubeSandbox 官方仓库已经提供 examples/openclaw-integration Skill，适合验证"Agent 能否主动把代码放进 MicroVM"。企业版本更适合把 SDK 调用封装成固定 Plugin Tool：模型只提供命令、文件和策略档位，不能自由拼接 CubeAPI 管理请求。本文已经按 OpenClaw 官方 defineToolPlugin 接口实现并跑通这条路径，代码见 [openclaw-plugin](https://github.com/runzhliu/aik8s/tree/main/examples/cubesandbox-openclaw-dsh-direct/openclaw-plugin)。

OpenClaw 当前公开文档列出的内置 Sandbox backend 主要是 Docker、SSH 和 OpenShell，不能把 CubeSandbox 当作已经原生支持的第四种 backend。若没有经过验证的 backend 接口，先注册独立 Cube 工具并对不可信会话禁用宿主 exec，比深度修改 Gateway 更容易跟随上游升级。

### 3.2 DSH：先用 Cordis Tool Plugin 硬路由，再演进到 Provider

DSH 的可组合设计很适合接远端执行面，但接入位置要正确：

| DSH 能力 | CubeSandbox 映射 |
|:--|:--|
| ctx.shell | sandbox.commands.run() |
| ctx.fs | sandbox.files.read/write/list/... |
| ctx.terminals | sandbox.pty |
| 代码解释器 | sandbox.run_code() |
| Session 初始化 | Sandbox.create() 或连接现有实例 |
| 空闲回收 | lifecycle.on_timeout=pause/kill |
| 任务取消 | 终止 command / PTY，必要时 Kill Sandbox |

DSH 的审批、read-only / workspace-write / danger-full-access 语义仍留在控制面。Provider 根据已批准策略决定要调用哪一个沙箱能力；CubeSandbox 再执行资源、网络和 MicroVM 隔离。

本文先实现了一个可落地的中间层：Cordis Plugin 通过 ctx.tools.register 注册四个 Cube 工具，并用 Profile Patch 禁用 tool-bash、tool-pwsh、tool-fs、tool-fs-search 和 tool-str-replace-editor，强制模型的命令与文件操作只走 Adapter。代码见 [dsh-plugin](https://github.com/runzhliu/aik8s/tree/main/examples/cubesandbox-openclaw-dsh-direct/dsh-plugin)。它已经摆脱 Skill 和宿主包装脚本，但工具名仍是 cube\_\*，还不是对 DSH 原有 Bash / Editor / Terminal 的透明替换。

不要只把 CubeSandbox 实现成 ctx.sandbox。DSH 官方明确把这个接口定义为 same-world confinement；最终的远端 MicroVM Provider 应替换环境一致的一组 Shell、文件和终端能力，否则同一轮工具可能一半在本机、一半在 VM，工作目录和权限语义会失真。

## 四、WebUI 中已经能看到 OpenClaw 方向

CubeSandbox v0.7.0 WebUI 的"数字助手"页面已经把 OpenClaw 助手、模型服务、助手模板和团队共享放进同一入口。页面也清楚标注了 Preview，适合演示和早期验证，不应把它误写成已稳定的企业多租户控制面。

![CubeSandbox WebUI 的数字助手页面](./assets/2026-09-30-openclaw-dsh/02-webui-digital-assistant.jpg)

*图 2：CubeSandbox WebUI 的数字助手页面*

如果选择这条路径，仍要在外围补齐 SSO、租户、审批、模型与工具目录、预算、审计和发布流程。更稳妥的生产形态通常是：企业控制面管理多个受控 OpenClaw / DSH Runtime，Runtime 再调用 CubeSandbox 执行面。

## 五、实战：验证一个 Agent 会话需要的完整链路

### 5.1 前置条件

实验使用：

- Kubernetes v1.30.x；
- CubeSandbox v0.7.0；
- 一个 READY 的 sandbox-code 模板 alias；
- 本机可以访问 CubeAPI 和 CubeProxy；
- Python SDK 固定为 cubesandbox==0.7.0。

生产环境应使用受信任 DNS、TLS 和 API Key。下面的 127.0.0.1 只代表通过 kubectl port-forward 建立的本地实验入口：

准备 SDK：

这里没有设置 CUBE_PROXY_SCHEME：cubesandbox==0.7.0 的 Python SDK 不读取这个变量。连接参数升级后也要回到对应版本 SDK 源码或官方说明核对，不能凭其他语言 SDK 的变量名推断。

仓库中的完整脚本是 [scripts/cubesandbox_openclaw_dsh_smoke.py](https://github.com/runzhliu/aik8s/blob/main/scripts/cubesandbox_openclaw_dsh_smoke.py)。它不启动 OpenClaw 或 DSH，而是直接验证两者的 Adapter 必须依赖的执行面契约。

### 5.2 创建一个受控会话沙箱

核心创建参数如下：

这些字段对应一个合理的企业默认值：

- 会话空闲后暂停，而不是一直占用计算资源；
- 不允许任意访问公网；
- 外部访问沙箱服务必须携带 traffic token；
- metadata 只放追踪和策略标签，不放用户隐私或密钥。

Chrome 中可以看到实时运行数变成 1。截图只保留计数区域，沙箱 ID、模板 ID 和节点地址均已裁掉：

![WebUI 实时运行数变成 1](./assets/2026-09-30-openclaw-dsh/03-running-count.jpg)

*图 3：WebUI 实时运行数变成 1*

### 5.3 执行 Shell、文件和 Python

测试模板中的 command 默认以 Guest UID 0 运行。MicroVM 内的 root 不等于宿主机 root，但企业模板仍应优先使用非 root 用户、最小软件集和只读基础层，避免在 Guest 内无意义地扩大权限。

### 5.4 验证出站和入站边界

脚本尝试从沙箱连接公网地址，结果失败，因此 allow_internet_access=False 生效。

随后直接访问沙箱数据面健康端点：

| 请求 | 返回 |
|:--|:--|
| 不带 e2b-traffic-access-token | 403 |
| 携带创建时返回的 token | 204 |

这证明"知道沙箱域名或 ID"不足以访问受保护服务。生产中还要同时限制 CubeProxy 网络入口，并加 TLS、API 鉴权、速率限制和审计。

### 5.5 暂停、恢复并验证状态

本次结果：

- 文件内容在 Resume 后仍是 session=demo, turn=1；
- Python 内存变量从 41 继续递增到 42；
- Pause 约 2.18 s；
- Resume 约 2.72 s。

可观测性页面同时显示一个运行中沙箱。截图结束后该临时沙箱已经销毁：

![可观测性页面显示一个运行中沙箱](./assets/2026-09-30-openclaw-dsh/04-observability.jpg)

*图 4：可观测性页面显示一个运行中沙箱*

### 5.6 实测输出

脚本输出经过脱敏后如下：

finally 中始终调用 kill()。完成截图后又调用 Sandbox.list()，集群沙箱数为 0。

## 六、两个真实故障：决定体验是否"丝滑"的细节

### 6.1 fresh install 的 CubeProxy admin token 可能不一致

第一次执行 Pause → Resume 时，恢复失败：

v0.7.0 Chart 的 helper 注释已经解释原因：当 lifecycleManager.adminToken 为空时，全新安装的单次渲染可能让 release Secret 和 CubeMaster 配置各自生成一次随机值；下一次 Helm upgrade 才会通过 lookup 复用已存在的 Secret。

实验环境使用原值升级，并让通过 subPath 挂载配置的 CubeMaster 重启：

生产安装更稳妥的做法是在发布系统中生成至少 16 字符的随机值，安全注入 lifecycleManager.adminToken，确保同一次渲染只有一个来源；不要把真实 Token 提交到 Git。

修复后可以只比较摘要，不输出 Token 本身：

还要注意：Secret 内容更新不代表使用 subPath 的进程已经读取新文件。没有触发 Pod Template checksum 时，应显式滚动 CubeMaster。

### 6.2 connect() 后不能丢失 traffic token

开启 network.allow_public_traffic=false 后，v0.7.0 Python SDK 只在 create() 响应中返回 traffic token。Sandbox.connect() 返回的新对象不包含它；如果 Adapter 只保存 sandbox_id，恢复后的文件与命令调用会被 CubeProxy 以 403 拒绝。

因此企业 Adapter 必须：

1. 创建时同时保存 sandbox_id 与 traffic_access_token；
2. Token 在数据库中加密，日志和 Trace 中只保留摘要；
3. 每个数据面请求都携带 Token；
4. Kill 后立即删除映射与 Token；
5. SDK 升级后重新验证 Connect / Resume 行为。

本次脚本使用保留原始 SDK 对象的 resume() 完成测试，所以原 traffic token 仍在内存中。跨进程恢复时不能依赖这一点。

## 七、企业 Adapter 的最小设计

不要一开始就实现完整 IDE、浏览器和所有 E2B API。第一版只需覆盖 OpenClaw / DSH 最常用的五个操作：

这次已经把最小设计写成可运行参考实现：[examples/cubesandbox-openclaw-dsh-direct](https://github.com/runzhliu/aik8s/tree/main/examples/cubesandbox-openclaw-dsh-direct)。调用关系不是"Plugin 直接持有 Cube 管理权限"，而是：

Adapter 当前实现了这些防线：

- 模型只能请求平台预置的 offline-code，不能传模板 ID、CIDR、公开流量或生命周期配置；
- 会话键只在 Runtime 与 Adapter 之间传递，落日志的是带密钥的 HMAC-SHA-256 摘要；
- 文件限制在 /workspace 与 /tmp，拒绝路径穿越，并限制请求、命令、文件、输出和超时大小；
- Cube API Key、traffic token 和完整 Sandbox ID 只留在 Adapter；opaque lease 只在 Plugin 与 Adapter 的控制链路流转，模型结果只返回 8 字符 sandbox_ref；
- 审计只记录 Runtime、动作、短引用、请求 ID、耗时、结果和命令/路径摘要，不记录命令正文、文件内容、stdout、stderr 或 Token；
- /audit 演示页默认关闭，生产应把 JSONL 送进不可变审计管道。

这仍是参考实现，不是现成的多租户控制面。租约目前保存在进程内，因此示例 Deployment 明确只运行 1 个副本；做高可用前必须把 lease、加密 traffic token 与 owner fencing 放进持久化服务，或让会话稳定路由到唯一 owner。当前也没有 PTY、流式输出、取消、租户配额、审批回调和跨进程恢复，不能仅凭一次实测就宣称已生产就绪。

### 7.1 策略档位

不要让模型自由提交任意 CIDR 和 host mount。由平台维护有限的策略档位：

| Profile | 网络 | 工作区 | 适合场景 |
|:--|:--|:--|:--|
| offline-code | 完全断网 | 临时卷 | 数据处理、未知脚本 |
| repo-build | 只允许内部 Git、软件镜像和制品库 | Session Volume | 编译与测试 |
| web-research | 仅 HTTP/HTTPS，经 L7 审计 | 临时卷 | 浏览与资料提取 |
| model-tool | 只允许 Model / Tool Gateway，代理注入凭据 | 临时卷 | Agent 子任务 |
| approved-release | 仅批准的发布端点 | 受控 Volume | 需要人工审批的发布任务 |

模型可以请求某个 Profile，最终选择由策略引擎和人工审批决定。

### 7.2 状态与工作区

推荐把状态分开：

不要把完整宿主 home、SSH 目录、云凭据目录或 Docker Socket 挂进 MicroVM。确实需要共享数据时，使用只读 Volume、对象存储或受控上传接口。

### 7.3 凭据

优先顺序应是：

1. Tool Gateway 根据用户和动作签发短期身份；
2. CubeEgress 只向匹配的 HTTPS Host / SNI 注入 Header；
3. 只读文件或内存注入短期 Token；
4. 最后才考虑环境变量。

模型 API Key、Git Token 和云凭据不应写进模板、快照、命令行、metadata 或普通日志。

## 八、用 CubeSandbox 具体怎么玩 OpenClaw 与 DSH

下面不是功能清单，而是从十分钟 PoC 到企业 Adapter 的实际玩法。每一项都给出操作、提示词和验收点。

### 8.1 OpenClaw 实战：模型自动调用 Skill，在 MicroVM 内执行

CubeSandbox 官方仓库已经提供 OpenClaw Skill。先把它安装到目标 OpenClaw workspace：

Skill 本身不是一个远程执行协议，也不会自动改写 OpenClaw / DSH 的 Shell Provider。它的原理是：Runtime 把 SKILL.md 作为操作说明提供给模型，模型再调用宿主工具启动包装脚本；包装脚本使用 Cube SDK，SDK 根据 CUBE_API_URL 访问 CubeAPI，并通过 CUBE_PROXY_NODE_IP / CUBE_PROXY_PORT_HTTP 访问 Sandbox 数据面，最终由 CubeSandbox 调度 MicroVM 执行任务。因此，看到模型"读了 Skill"只能证明它选择了这套操作说明，还要用 Sandbox 实时实例、SDK 结果和清理状态证明任务确实进入 MicroVM。

再把 CubeAPI、CubeProxy、Template 和 API Key 作为 OpenClaw 进程环境配置，不要写进 SKILL.md 或 Agent Prompt。官方示例使用 E2B 兼容环境变量；新项目也可以直接使用 cubesandbox SDK。

本文还提供了一个只保留执行、断网验证和清理逻辑的[最小 OpenClaw Skill](https://github.com/runzhliu/aik8s/tree/main/examples/cubesandbox-openclaw-dsh/openclaw-skill/cube-sandbox)。它把用户代码写进远端 MicroVM，包装脚本本身只接收代码和平台预置策略，不允许模型自己拼装 CubeAPI 管理请求。

给 OpenClaw 一个明确任务：

验收时不要只看最终数字，还要检查：

- CubeSandbox WebUI 的运行中数量从 0 → 1 → 0；
- Agent 没有在 OpenClaw Gateway 本地创建 /tmp/input.py；
- allow_internet_access=false 确实阻止连接；
- 异常路径仍执行 Kill。

这条路径适合十分钟 PoC。它通常仍需要 OpenClaw 在本地用 exec 启动 Python SDK，因此不能把"安装了 Skill"当成宿主执行已经关闭。企业版应继续封装 Plugin Tool。

这次不是只验证包装脚本，而是启动了完整 OpenClaw Gateway 和 Control UI：

- OpenClaw：2026.7.1 官方容器镜像；
- 模型：openai/gpt-5.6-sol，通过官方支持的 ChatGPT / Codex 设备登录；
- Skill：以 openclaw-workspace 来源加载，状态为 eligible=true、modelVisible=true；
- Cube SDK：cubesandbox==0.7.0；
- 模板 alias：agent-code；
- 网络策略：禁止公网出站，禁止公开流量。

在 Chrome 的 OpenClaw Control UI 中发送上面的任务后，模型先读取 SKILL.md，再调用包装脚本。页面中的 Activity: 2 tools 是真实工具活动，不是事后拼接的日志：

![OpenClaw Control UI 中的工具活动](./assets/2026-09-30-openclaw-dsh/05-openclaw-activity.jpg)

*图 5：OpenClaw Control UI 中的工具活动*

展开工具活动可以看到两次 Bash 调用：第一次读取 Skill，第二次运行 cube_agent_task.py。用户 Python 代码没有在 Gateway 容器本机执行：

![两次 Bash 调用的展开细节](./assets/2026-09-30-openclaw-dsh/06-openclaw-tools.jpg)

*图 6：两次 Bash 调用的展开细节*

本次 Agent 会话的实际结果为：

任务完成后再次调用 Sandbox.list()，返回空列表。由此可以同时证明：OpenClaw 真的选择了 Skill、代码真的进入 MicroVM、断网策略生效、异常安全清理路径没有留下活动沙箱。

### 8.2 OpenClaw 玩法二：一会话一沙箱，隔天回来还能继续

实现一个内部 Plugin，固定暴露以下工具：

Plugin 从当前 OpenClaw sessionKey 查 lease，模型不直接传 sandbox_id 或 traffic token。建议流程：

1. 第一次收到代码任务时创建 Sandbox；
2. 把 sessionKey → sandbox_id + encrypted token 写入租约表；
3. 每次工具调用刷新 TTL；
4. 空闲五分钟自动 Pause；
5. 下一条消息自动 Resume；
6. 用户关闭会话、管理员回收或最长生命周期到期时 Kill。

可以用两轮对话验证"丝滑感"：

通过条件：第二轮不重新上传项目，能够读取第一轮文件；WebUI 出现 paused → running；OpenClaw 重启后仍能通过租约表找回同一会话。

对不可信群聊，可以禁用宿主 exec/read/write，只保留经过审计的 Cube 工具；主会话是否允许更高权限，应由独立 Tool Policy 决定，不要只靠提示词。

### 8.3 OpenClaw 玩法三：Digital Assistant 做快照、克隆与回滚

CubeSandbox WebUI 的数字助手路线会把整个 OpenClaw Runtime 做成助手模板：

1. 在"模型服务设置"配置 Provider、Base URL、Model 和受管 API Key；
2. 从模板市场准备轻量版或 all-in-one OpenClaw 助手模板；
3. 创建个人助手；
4. 安装 Skill、配置渠道或修改 Agent 指令；
5. 在稳定点创建 Snapshot；
6. 修改失败时 Rollback；
7. 从稳定快照 Clone 一个新助手，再比较两套配置。

适合玩的实验包括：

1. 一个基础助手克隆出"研发""运维""数据分析"三个角色；
2. Skill 升级前做快照，验证失败后秒级回退；
3. Clone 两个实例分别使用不同模型或 Prompt，做 A/B 评测；
4. 把个人实例转成团队共享前检查密钥、记忆和浏览器 Profile 是否被错误继承。

本次只在真实 WebUI 中验证了页面与准备步骤，没有配置模型 Key、没有创建 OpenClaw 实例。官方明确把 Digital Assistant 标为 Preview，这部分属于下一阶段实验，不列入本文"已通过"结果。

### 8.4 DSH 实战：DeepSeek V4 Pro 自动加载 Skill 并调用包装工具

DSH 同样可以先走轻集成：给它一个 Skill，要求遇到不可信 Shell、仓库或附件时调用固定的 cube-run 包装工具。包装工具内部使用 Cube SDK，DSH 只看到稳定参数：

一个适合实际玩的提示词：

这一步改动小，适合先验证网络、文件语义和长命令输出。它的缺点是 DSH 内置的 Shell、文件工具与 cube-run 是两套表面，模型可能选错。要做到无感，下一步应写 Provider。

本文的真实验证使用正在运行的 DSH 0.1.1-rc.2，在 Chrome WebUI 新建会话并选择 DeepSeek V4 Pro。安装的[最小 DSH Skill](https://github.com/runzhliu/aik8s/tree/main/examples/cubesandbox-openclaw-dsh/dsh-skill/cube-sandbox)与 OpenClaw 版本遵守同一个结果契约。模型按以下顺序完成任务：

1. 根据提示自动调用 Skill cube-sandbox；
2. 读取包装脚本，确认参数和 finally 清理；
3. 调用 Bash 运行脚本；
4. 等待 CubeSandbox 返回结构化 JSON；
5. 用中文整理执行器、结果、断网状态、创建耗时和清理结果。

对话页同时保留 Skill、Read、Bash 三类活动和最终结果：

![DSH 对话页保留三类活动和最终结果](./assets/2026-09-30-openclaw-dsh/07-dsh-conversation.jpg)

*图 7：DSH 对话页保留三类活动和最终结果*

DSH 的轨迹页能把 Input、Model、Tools 放在同一时间线上。工具行中可以看到 Skill 加载、脚本读取、Bash 调用以及返回给模型的 JSON：

![DSH 轨迹页的时间线](./assets/2026-09-30-openclaw-dsh/08-dsh-trajectory.jpg)

*图 8：DSH 轨迹页的时间线*

本次 DSH Agent 会话的实际结果为：

为了避免只凭最终回答判断，又让 DSH 执行了一次 60 秒保持任务。轨迹中可以看到 Skill、脚本读取、Bash 工具结果和 sandbox_ref=7fddceaa；保持期间，CubeSandbox WebUI 同时出现前缀、后缀一致的运行中 Sandbox：

![DSH 60 秒保持任务的轨迹](./assets/2026-09-30-openclaw-dsh/09-dsh-hold-result.jpg)

*图 9：DSH 60 秒保持任务的轨迹*

![WebUI 中前缀、后缀一致的运行中 Sandbox](./assets/2026-09-30-openclaw-dsh/10-dsh-hold-webui.jpg)

*图 10：WebUI 中前缀、后缀一致的运行中 Sandbox*

这证明 DSH 可以先不改 Runtime 核心，通过 Skill 把一类高风险任务显式路由到 CubeSandbox。它仍是轻集成：若要让 Bash、编辑器和 Terminal 默认处于同一个远端工作区，需要继续实现下一节的原生 Provider。

### 8.5 不经过 Skill：OpenClaw / DSH 直接调用受控 Adapter

Skill 路线证明了模型会主动选择 CubeSandbox，但它通常还需要宿主 Bash 启动包装脚本。为了证明"没有 Skill 也能真的创建 MicroVM"，这次实现了共享 Adapter 和两个 Runtime Plugin：

- adapter：唯一持有 Cube SDK 配置、完整 Sandbox ID 和 traffic token；
- openclaw-plugin：使用 OpenClaw 官方 defineToolPlugin；
- dsh-plugin：使用 Cordis ctx.tools.register，并附带禁用宿主工具的 Patch；
- deploy/kubernetes.yaml：单副本、非 root、只读 rootfs、Secret 与 NetworkPolicy 的参考清单。

Adapter 对 Runtime 暴露的是 HTTP API，内部再调用 cubesandbox==0.7.0 SDK：

所有写请求都要求 Bearer Token；生产应再通过 Service Mesh 或 Gateway 加 mTLS、工作负载身份、速率限制与授权。acquire 按 (runtime, HMAC(session_key)) 幂等，因此同一会话的 exec/read/write 进入同一个 MicroVM。Plugin 需要内部 lease 才能调用下一步，但 lease、完整 Sandbox ID 和 traffic token 都不进入模型结果。

启动 Adapter 的最小方式如下，Token 应来自 Secret Manager：

CUBE_ADAPTER_HMAC_KEY 应与 Bearer Token 分开管理：前者保持审计关联稳定，后者可以常规轮换。两者都不应出现在插件配置、模型上下文或普通日志中。

**OpenClaw Tool Plugin 实测**

安装后要同时允许插件和它注册的工具：

如果已有 plugins.allow 或 tools.alsoAllow，应合并现有受信项，不能照抄命令覆盖。实测中出现过一个很有迷惑性的状态：插件检查显示 loaded，但缺少 tools.alsoAllow 时模型完全看不到四个工具。Gateway 进程只需配置 CUBE_ADAPTER_URL 和 CUBE_ADAPTER_TOKEN，不需要 Cube API Key。

给真实模型的提示明确要求"不读取 Skill、不使用宿主 exec，只调用 cube_exec，完成后 cube_release(action=kill)"。OpenClaw Activity 显示恰好两次工具调用，结果为 openclaw-direct-ok、退出码 0、短引用 45a28df5：

![OpenClaw Tool Plugin 实测的两次工具调用](./assets/2026-09-30-openclaw-dsh/11-openclaw-plugin.jpg)

*图 11：OpenClaw Tool Plugin 实测的两次工具调用*

这证明 Tool Plugin 路线可行，但 OpenClaw 当前稳定公开接口没有"任意第四种原生 Sandbox Backend"。参考实现没有伪装成 Docker / SSH / OpenShell backend；企业 Profile 还应显式拒绝宿主 exec/read/write，避免模型绕过 Cube 工具。

**DSH Cordis Plugin 实测**

安装插件并应用参考 Patch：

Patch 禁用了模型侧的 Bash、PowerShell、FS、FS Search 与字符串编辑器，再注册同名的四个 Cube 工具。DSH 可从环境变量读 Token，也可从只读 Secret 文件读取；容器部署更适合 tokenFile。本地 file: 安装会把插件复制到 DSH 插件仓库，修改源码后必须重新执行 add/update，单纯重启不会刷新已安装副本。

真实 DeepSeek V4 Pro 会话也被要求不读 Skill、不碰宿主工具。轨迹里只有 cube_exec 与 cube_release，执行输出 DSH_DIRECT_V2=338350、退出码 0、耗时 35127 ms，短引用为 f795f7fc：

![DSH Cordis Plugin 实测轨迹](./assets/2026-09-30-openclaw-dsh/12-dsh-plugin.jpg)

*图 12：DSH Cordis Plugin 实测轨迹*

在 cube_exec 保持运行的 35 秒内，CubeSandbox WebUI 同时出现 f795f7…f099。WebUI 显示的是同一完整 ID 的前后缀，Agent 与审计只公开前 8 字符：

![WebUI 显示同一完整 ID 的前后缀](./assets/2026-09-30-openclaw-dsh/13-dsh-webui-id.jpg)

*图 13：WebUI 显示同一完整 ID 的前后缀*

最后，Adapter 审计页同时出现 OpenClaw 45a28df5 与 DSH f795f7fc 的 acquire、exec、release。每个 Tool 先幂等 acquire，所以同一引用可能出现多条 acquire；页面不含命令正文、输出、Token、原始 session key 或完整 Sandbox ID：

![Adapter 审计页交叉验证](./assets/2026-09-30-openclaw-dsh/14-adapter-audit.jpg)

*图 14：Adapter 审计页交叉验证*

由此形成了可核验的三点联证：Agent 轨迹证明模型调用了什么工具，CubeSandbox 实时页证明 MicroVM 确实存在，Adapter 审计证明请求、Runtime、结果和耗时可关联。任务结束后 Sandbox.list() 再次为 0。

这条 DSH 路线已经不依赖 Skill，也能硬禁用常见宿主工具；但模型仍看到 cube\_\*，因此它是"直接 Tool Plugin"，不是下一节所说的透明 Provider。

### 8.6 DSH 玩法三：把 shell/fs/pty 换成 Cube Provider

Provider 版让模型继续使用 DSH 原来的 Bash、编辑器和 Terminal，不需要学一组 cube\_\* 工具：

建议按这个顺序实现：

1. 一次性 shell.exec；
2. fs.read/write/list/stat；
3. cwd、环境变量、超时、取消和完整 stdout/stderr；
4. PTY、resize、stdin 和后台任务；
5. Pause、Resume、重连和 Runtime 重启恢复；
6. 把 DSH approval 结果映射到平台维护的 network / workspace Profile。

测试重点不是"命令返回 0"，而是环境一致性：Bash 写出的文件必须立即能被 editor 读到；PTY 与一次性 Bash 要处于同一 Sandbox；workspace-write 不能意外写到 DSH 宿主机。

### 8.7 两边都很好玩：两个 Agent 并行改同一问题

Snapshot 与 Clone 很适合 OpenClaw 子 Agent 或 DSH 多方案并行：

仓库提供了完整脚本 [scripts/cubesandbox_agent_parallel_clone_demo.py](https://github.com/runzhliu/aik8s/blob/main/scripts/cubesandbox_agent_parallel_clone_demo.py)。本次在同一基线上完成：

1. 写入 baseline；
2. 创建 Snapshot；
3. 故意写入 unsafe-change 后 Rollback；
4. 并发 Clone 两个沙箱；
5. Clone A 写入 minimal-fix，Clone B 写入 refactor；
6. 验证两个 Clone 互不影响，基线仍为 baseline；
7. 销毁三个沙箱。

实测样本：

这只是两路功能样本，但已经证明了一个很有价值的 Agent 模式：不要让多个方案在同一工作区互相覆盖，用 Clone 形成真正独立的执行分支。

### 8.8 浏览器 Agent 与红队玩法

可以构建带 Chromium 和 Playwright/CDP 的模板，让每个浏览器任务进入独立 MicroVM。适合测试：

- 打开含 Prompt Injection 的网页后，能否访问内网元数据地址；
- CDP 与 noVNC 不带 traffic token 是否返回 403；
- 下载文件是否只能落到 Session Workspace；
- 浏览器 Profile、Cookie 和剪贴板是否跨租户泄漏；
- 页面关闭后 Chromium、/dev/shm 和转发端口能否回收；
- 浏览器等待用户确认时 Pause，下一轮 Resume 后页面是否保留。

OpenClaw 的远端 SSH/OpenShell backend 当前不提供完整的 sandbox browser 能力，因此不能从"Shell 能远端执行"推导出"浏览器也能无缝迁移"。

### 8.9 性能与可靠性玩法

最后再把功能实验升级为平台压测：

- 1、10、50、100 并发 Create / Pause / Resume / Kill；
- 不同模板体积的 P50、P95、P99；
- 同一 Snapshot 并发 Clone 多个 Agent；
- OpenClaw / DSH Runtime 重启后重连原 Sandbox；
- Snapshot 后端延迟和故障；
- CubeProxy 缓存、节点隔离与网络抖动；
- 孤儿 Sandbox、Volume、Snapshot 和 lease 自动回收；
- 升级前后 traffic token、网络策略和快照兼容性。

## 九、企业审计：页面没有历史时去哪里看

CubeSandbox WebUI 的"沙箱"页写的是"所有运行中微虚拟机的实时视图"。它不是历史执行列表：kill() 完成后，实例会从页面和 Sandbox.list() 中消失。因此，页面从 0 → 1 → 0 适合证明实时生命周期，却不能承担企业审计。

完整证据要分层保存：

| 层级 | 能回答的问题 | 原始来源 | 不能单独证明什么 |
|:--|:--|:--|:--|
| OpenClaw / DSH | 谁发起、模型是谁、选择了哪个 Skill / Tool、参数与审批、返回给用户的结果 | OpenClaw Activity、DSH Trajectory、Runtime 会话日志 | MicroVM 是否真的创建、网络策略是否真的执行 |
| CubeMaster / CubeShim / VMM | 哪个 Sandbox 何时创建、启动、暂停、恢复、销毁，模板和资源是什么 | 控制面日志与节点 /data/log/CubeShim、/data/log/CubeVmm | Shell 命令正文和业务身份 |
| CubeProxy | Sandbox 调用过文件、进程、PTY 等哪些数据面 API，状态码和耗时是什么 | 节点 /data/log/cube-proxy/access.log | /process.Process/Start 的请求体、命令正文、stdout / stderr |
| CubeEgress | HTTP/HTTPS 出站被哪条规则放行或拒绝，目标、路径、状态和延迟是什么 | 节点 /data/log/cube-egress/access.jsonl | 未进入 L7 代理的普通 L3/L4 流量；完整命令输出 |
| MySQL 实例记录 | 历史上有哪些实例、创建和删除时间是什么 | t_cube_instance_info、t_cube_instance_userdata | Agent 对话、具体命令和输出 |

### 9.1 本次 OpenClaw 任务的真实交叉证据

为了把 Agent 回答与 CubeSandbox 页面做现场联证，又让 OpenClaw 运行了一次 60 秒保持任务。OpenClaw 返回 sandbox_ref=d9aafb40，同一前缀同时出现在 WebUI 的运行中 Sandbox 行：

![OpenClaw 60 秒保持任务](./assets/2026-09-30-openclaw-dsh/15-openclaw-hold.jpg)

*图 15：OpenClaw 60 秒保持任务*

![WebUI 运行中 Sandbox 行出现同一前缀](./assets/2026-09-30-openclaw-dsh/16-openclaw-hold-webui.jpg)

*图 16：WebUI 运行中 Sandbox 行出现同一前缀*

再使用完整 Sandbox ID 在节点日志中检索，得到一条可以互相对齐的时间线：

| 相对时间 | 证据 |
|:--|:--|
| T+0 ms | CubeShim create req start |
| T+40 ms | Guest agent ready，MicroVM 启动完成 |
| T+约 200 ms | CubeProxy POST /files，写入 Agent 任务文件 |
| T+约 300 / 500 ms | 两次 POST /process.Process/Start，分别运行任务和断网检查 |
| T+60.6 s | CubeShim 收到 Kill，随后 destroy sandbox finish |

这组证据比只看 Agent 的最终回答强：同一个 ID 同时出现在 OpenClaw 结果、CubeSandbox 实时列表、CubeProxy 和 CubeShim 中。但它仍不等于完整命令审计，因为 CubeProxy access log 不记录进程启动请求体和 stdout。命令、参数、输出、用户、模型和审批必须由 Agent Adapter 额外写结构化审计事件。

### 9.2 直接查询节点日志

先拿到运行目标节点上的 cube-node Pod，再用完整 Sandbox ID 检索。多节点环境要根据实例信息中的 Node 定位对应 DaemonSet Pod，不能默认只查第一个：

实际集群还可能出现 Cubelet-req.log、Cubelet-stat.log、cube-proxy/error.log 等文件。它们更适合故障定位，不应代替业务审计事件。节点文件一旦轮转、Pod/节点被清理或磁盘损坏就可能丢失，生产环境不能等到出事后再临时 grep。

### 9.3 CubeEgress 出网审计的边界

创建 Sandbox 时，为 L7 Rule 设置 action.audit：

- none：不落审计；
- metadata：默认值，记录时间、Sandbox IP、目标 IP/端口、scheme、Host、method、path、状态、字节、延迟、TLS 和 upstream；
- full：v0.7.0 仍等同 metadata，不能据此宣称已采集请求或响应 Body。

只有进入 CubeEgress 的 HTTP/HTTPS L7 流量会写入 access.jsonl。例如 allow_internet_access=false 在 L3/L4 直接拦截一个原始 TCP 连接时，文件里不会自动出现对应 L7 记录。若企业要求"每一次出站尝试都有记录"，还要汇聚 eBPF、主机防火墙或 CNI Flow Log。

本次实验还发现一个必须在上线验收中拦住的问题：虽然 cube-node Pod 最终显示 Running，cube-egress-net 的探针曾报告 iptables ... nf_tables ... TRANSPROXY ... incompatible，容器也持续出现 rule reapply failed，实际 access.jsonl 保持为空。也就是说，**Pod Running 不等于 L7 审计可用**。生产验收至少要创建一条 audit=metadata 的确定性 deny 规则，发起请求，并同时确认：

- 请求被 403 拒绝；
- access.jsonl 新增相同请求的 JSONL；
- cube-egress-net readiness 正常且没有规则重放错误；
- iptables legacy / nftables 后端与宿主机已有规则一致。

这次 L3/L4 完全断网验证仍然成功，但不能拿它替代尚未通过的 L7 审计验收。

### 9.4 为什么销毁后数据库也查不到

CubeMaster 的 common.disable_hard_delete 默认为 false。默认删除路径会硬删除 t_cube_instance_info，所以 WebUI 和普通数据库查询都看不到已销毁实例。本次实验环境未配置该字段，并且在 Kill 后按完整 Sandbox ID 查询得到 0 行，行为与默认值一致。

需要保留实例墓碑用于审计或恢复时，可以在 CubeMaster 配置中启用：

v0.7.0 中，该设置会让实例信息改为软删除，并且其优先级高于 soft_delete_purge：t_cube_instance_info 和 t_cube_instance_userdata 不会被墓碑清理器删除。启用前要做容量、索引、访问控制、数据保留期和隐私评估。它只保留实例记录，并不会自动补齐命令、输出、用户和审批历史。

### 9.5 推荐的企业审计事件

Adapter 在每次调用 CubeSandbox 前后都应写一条追加式结构化事件，至少包含：

不要把长期 Token、Cookie、Authorization Header、完整个人信息或无限量 stdout 直接写进日志。更合理的方式是：有限长度且经过脱敏的摘要进入检索系统，完整产物加密写入对象存储，审计事件只保存摘要和对象引用。

推荐汇聚链路如下：

![审计汇聚链路](./assets/2026-09-30-openclaw-dsh/17-audit-pipeline.jpg)

*图 17：审计汇聚链路*

还要统一 NTP 和时区、给日志加 tenant_id + session_id + sandbox_id + trace_id、限制审计检索权限、记录查询行为，并设置在线检索与归档的独立保留期。v0.7.0 的 CubeProxy 本地脚本按约 500 MiB 阈值轮转且只留少量归档；CubeEgress 的 access.jsonl 也不能被当作永久存储，因此日志采集必须在节点本地轮转或故障前完成。

## 十、生产上线检查表

- OpenClaw / DSH 与 Sandbox 位于不同信任边界；
- 不可信 Agent Profile 已禁用宿主 Shell / FS / Editor，且模型工具列表中确实只剩受控远端工具；
- OpenClaw 的 plugins.allow、tools.alsoAllow 与 DSH 最终合成 Profile 已在启动后验证；
- 每个租户或会话有独立 Sandbox lease；
- Adapter Bearer Token 来自 Secret Manager，服务间使用 mTLS / 工作负载身份并可轮换；
- 多副本前已实现持久 lease、加密 traffic token、owner fencing 与 Runtime 重启恢复；
- CubeAPI、CubeProxy、WebUI 和运维端点均有认证与 TLS；
- 默认拒绝出站，只允许企业 Git、镜像、软件源和 Tool / Model Gateway；
- traffic token 与 sandbox_id 一起加密保存；
- 不向 Sandbox 注入长期模型或云凭据；
- 禁止宿主目录、Docker Socket 和高权限设备的任意挂载；
- 模板固定版本或 Digest，经过扫描、SBOM、签名和回归；
- 资源、并发、TTL、快照和 Volume 都有租户配额；
- Pause / Resume、Connect、Kill 与异常清理都是幂等操作；
- Agent、Sandbox、网络代理和外部工具日志可以用 Trace ID 关联；
- 已用真实 L7 allow / deny 请求验证 CubeEgress JSONL，而不只是检查 Pod Running；
- 已明确命令正文、stdout / stderr 的脱敏、摘要、加密归档与保留期；
- 已验证节点日志轮转前可被采集，审计索引与 WORM 归档均可查询；
- 若要求保留已销毁实例，已评估并启用 disable_hard_delete；
- 红队测试覆盖 Prompt Injection、数据外传、内网探测和跨会话访问；
- 集群升级、Token 轮换、模板升级和快照不兼容都有回滚预案。

## 十一、结论

CubeSandbox 对 OpenClaw / DSH 的最大价值，不是"又多一种部署方式"，而是让 Agent Runtime 不再直接等于执行环境。

最推荐的企业路线是：

1. OpenClaw / DSH 留在受管 Runtime Pool；
2. 先用本文已实测的 Tool Plugin + 薄 Adapter，把命令与文件路由到 CubeSandbox；
3. 按会话管理 MicroVM，空闲 Pause，过期 Kill；
4. 用默认拒绝网络、traffic token 和代理注入保护数据与凭据；
5. 再补齐 PTY、流式取消、持久租约和透明 DSH Provider，最后评估整个 OpenClaw 助手进入 CubeSandbox 的 Preview 路径。

这样既保留 OpenClaw 的渠道与 Agent 生态、DSH 的可组合 Runtime，也把最危险的执行动作放进可观察、可回收、可快照的独立 MicroVM。本文的 Adapter 与两个 Plugin 已作为通用参考代码公开，不含私有集群、镜像仓库或账号假设；后续可以拆成独立项目并向上游贡献示例，但原生 OpenClaw backend 与透明 DSH Provider 仍需要先与各自社区确认稳定扩展契约。

**附：**

本次实测得到以下结果：

| 验证项 | 结果 |
|:--|:--|
| MicroVM 创建 | 成功；本次样本 134 ms |
| Shell / 文件 / Python | 成功 |
| 完全禁止公网出站 | 生效 |
| 沙箱公开入口不带访问令牌 | HTTP 403 |
| 携带访问令牌 | HTTP 204 |
| Pause | 成功；本次样本约 2.18 s |
| Resume | 成功；本次样本约 2.72 s |
| 暂停后的文件和 Python 内存 | 都保留 |
| OpenClaw Skill → CubeSandbox | 成功；openai/gpt-5.6-sol 自动调用 Skill，沙箱创建 164 ms |
| DSH Skill → CubeSandbox | 成功；DeepSeek V4 Pro 自动调用 Skill，沙箱创建 123 ms |
| OpenClaw → Adapter → CubeSandbox | 成功；模型只调用 cube_exec / cube_release，证据引用 45a28df5 |
| DSH → Adapter → CubeSandbox | 成功；宿主 Shell / FS 工具禁用，证据引用 f795f7fc |
| Adapter 审计 | 成功；Agent、实时 Sandbox 和审计事件可按短引用交叉验证 |
| 清理 | 沙箱销毁，集群沙箱数恢复为 0 |

这些时间只是一台节点、一个模板、一次请求的功能样本，不是性能基准。生产决策要继续测并发下的 P50、P95、P99、失败率和长尾。

本文严格区分已经完成和仍是路线图的部分：

| 范围 | 状态 |
|:--|:--|
| Cube SDK 创建、Shell、文件、Python、断网、traffic token、Pause / Resume、Snapshot、Rollback、Clone、Kill | 已实测 |
| CubeSandbox WebUI 数字助手、运行中沙箱和可观测性页面 | 已用 Chrome 登录态验证 |
| cube-sandbox Skill 在真实 OpenClaw 模型会话中触发 | 已实测；保留对话和工具活动截图 |
| cube-sandbox Skill 在真实 DSH 模型会话中触发 | 已实测；保留对话和完整轨迹截图 |
| OpenClaw 官方 Tool Plugin → 受认证 Cube Adapter | 已实现并实测；四个 cube\_\* 工具按 sessionKey 复用租约 |
| DSH Cordis Tool Plugin → 受认证 Cube Adapter | 已实现并实测；按 DSH Agent ID 复用租约，宿主执行插件可硬禁用 |
| Adapter 策略、限额、HMAC 会话引用与 JSONL 脱敏审计 | 已实现为可运行参考代码；当前只开放 offline-code |
| 创建 Digital Assistant / OpenClaw 实例并调用模型 | 未实测，官方功能为 Preview |
| DSH 原生透明 shell/fs/pty Provider | 未实现；本文实现的是模型直连 Cordis Tool Plugin，另给出 Provider 接入边界 |
| 浏览器 Agent、并发压测和跨节点恢复 | 未实测，列为下一阶段 |
