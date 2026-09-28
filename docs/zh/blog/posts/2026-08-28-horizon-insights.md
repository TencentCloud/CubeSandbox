---
title: "金融投研 Agent 沙箱化：弘则信息基于 CubeSandbox 的选型与架构实践"
date: 2026-08-26
author: 王正凯（弘则弥道首席技术官）
description: "金融投研 Agent 的一次 run，往往要持续几十秒到数分钟，期间会读取研究方法和数据定义、调用数据接口、运行脚本、生成中间文件、再由模型整理结果。承载它的沙箱不能只是跑一段代码，而是要把完整的 Agent 循环放进隔离环境。本文介绍 CubeSandbox 在弘则 4as Agent 中的三个核心使用场景。"
featured: false
---

# 金融投研 Agent 沙箱化：弘则信息基于 CubeSandbox 的选型与架构实践

作者｜弘则弥道首席技术官 · 王正凯

**编者按｜** 金融投研 Agent 的一次 run，往往要持续几十秒到数分钟，期间会读取研究方法和数据定义、调用数据接口、运行 Python 或 Node 脚本、生成中间文件、再由模型整理结果。执行过程中接触的动作、文件和外部服务无法在发布时完全枚举。这意味着，承载它的沙箱不能只是"跑一段代码"，而是要把完整的 Agent 循环放进隔离环境。

本文介绍 CubeSandbox 在弘则 4as Agent 中的三个核心使用场景：完整 Pi loop 在 Sandbox 内运行，DAS 共享资源通过 Host Mount 分发，模型及工具凭据通过 CubeEgress 在出口侧注入。随后说明本地化部署、接口兼容、性能和生产运维方面的实际体验。

## 一、业务背景：投研 Agent 的受控执行环境

弘则信息是弘则研究旗下的 AI 创新团队，服务于各类金融投资与研究机构。团队建设的 4as（All (Domain) Agents as a Service）是一套金融投研 Agent 产品，将领域知识、研究方法、数据工具和持续状态组织为可部署的研究 Agent。4as 已在多个商业合作项目中完成落地部署，面向更多机构用户的 Cloud 版服务也将于近期推出。

4as 中的研究 Agent 需要完成资料读取、数据处理、代码执行、文件生成、外部工具调用和多轮模型交互。单次任务通常持续几十秒到数分钟，执行过程会接触用户工作区和外部服务，具体动作也无法在发布时完全枚举。因此，系统需要处理以下约束：

- 每次 Agent run 具有独立的进程和文件边界；
- 领域资源按用户授权和版本挂载；
- 用户私有文件与平台共享资源分开保存；
- Sandbox 可以回收，业务状态和研究结果持续保留；
- 外部模型及数据服务可访问，长期凭据不进入 guest 环境；
- 同一套 Agent runtime 能够运行在中心环境和客户侧部署中。

其中，CubeSandbox 为 4as 提供了统一的沙箱执行底座。它为每次 Agent run 创建独立执行环境，承担运行模板、文件与命令执行、领域资源挂载、受控出网和 Sandbox 生命周期管理。此外，BWA（Bounded Workspace Agent）是 4as 的受控运行时，在 CubeSandbox 基础能力之上组织产品所需的权限、状态、工作区和交互协议。

## 二、技术架构：Pi loop 在 CubeSandbox 内运行

4as 的核心 Agent 执行基于 Mario Zechner 开源的 Pi coding agent SDK。团队将完整的模型—工具主循环称为 Pi loop，并在 Pi SDK 之上增加 scope、Workspace、任务生命周期、事件协议和 Sandbox adapter。

生产路径中的 Pi SDK 与完整主循环都运行在 CubeSandbox guest 内。一次 loop 包含 `session.prompt()` 驱动的模型往返、工具选择与执行、bash 和文件操作、MCP 工具调用、事件流以及会话状态推进。CubeSandbox 为这些动作提供同一个隔离的进程、文件和网络环境。

当前执行链路由四层组成：Host 提供面向用户的产品界面和后端接口，完成身份校验后将请求和 scope 转交 BWA Runtime。BWA 校验 tenant、user、DAS 和 thread，通过 E2B-compatible API 创建或复用 CubeSandbox 实例，管理 run 的状态、取消、超时、工作区路径和持久化 checkpoint。CubeSandbox 承载执行面，Pi runner 随 Sandbox template 进入 guest，负责具体的模型与工具循环。

![4as 投研 Agent 执行链路](./assets/2026-08-28-horizon-insights/01-agent-execution-flow.jpg)

*图 1：4as 投研 Agent 执行链路*

Guest 产生的 token、tool event、状态和文件变化先通过内部执行协议返回 BWA，BWA 再转换为自身稳定的 SSE 事件发送给前端。前端只依赖 BWA 契约，不感知 Cube Sandbox 的创建接口、动态 hostname 或内部 runner 协议，三层因此可以分别演进。这套分工的核心是将执行生命周期与业务生命周期分开：Cube Sandbox 承载可回收的计算过程，BWA 及其持久化存储保留用户工作区、研究产出和 thread 状态。沙箱销毁后，业务状态仍可在下一次运行中恢复。

## 三、Host Mount：按版本分发领域资源

DAS（Domain Analyst Service）是 4as 中的领域分析师服务。其共享定义包含领域知识、研究方法、数据引用、资产、确定性计算代码和 Function 定义。DAS 与普通 Skill 的区别在于：Skill 描述一类动作的执行方法，DAS 描述一个持续运行的领域分析师，即可分发的领域定义 + 受管理状态 + 状态转移机制。

DAS 的共享定义需要分发到每个执行环境。如果每次 run 都通过文件 API 把大型领域资源搬运进沙箱，开销会很高。我们用 Cube Sandbox 的 Host Mount 解决这个问题。Host Mount 负责分发其中共享、版本化、只读的部分。大致的分发链路是：

![DAS 分发链路](./assets/2026-08-28-horizon-insights/02-das-distribution.jpg)

*图 2：DAS 分发链路*

Host Mount 在这里的几个关键约束构成了分发机制的基础：每个 revision 对应一份不可变文件树，BWA 只为当前 run 授权的 DAS 生成挂载项，readOnly 固定为 true，宿主路径位于限定的共享目录中，路径白名单和只读属性共同约束挂载范围。这意味着 Pi 可以像读取本地文件一样消费这些资源，但运行中的 Agent 无法原地修改已发布版本——领域能力升级只能通过 FCP 发布新 revision 完成。DAS 的可变部分走另一条路径：用户私有工作区、实例当前状态和状态转移产生的结果由运行时持久化，不写回共享挂载目录。运行状态与共享定义由此形成两类独立所有权，前者属于具体实例，后者属于可分发版本。

这种分发方式解决了几个实际问题：

- 大型领域资源无需在每次 run 中通过文件 API 重复搬运；
- 多个 Sandbox 可以消费同一份宿主缓存，文件内容仍按 revision 固定；
- 授权结果直接体现在挂载清单中，未挂载的领域资源对 guest 不可见；
- 共享定义保持只读，Agent 产出统一进入私有工作区；
- 中心环境和客户侧环境使用相同的 revision 与目录语义。

Host Mount 因而成为 DAS 分发链路的一部分。它连接了 4as 内容与版本控制面 FCP 的发布流程和 Cube Sandbox 内的实际执行，同时保持共享定义、实例状态和用户文件之间的边界。

## 四、CubeEgress：在出口侧管理凭据

Sandbox 内的 Pi 需要访问模型服务、MCP 服务和部分数据接口。将长期 API key 写入 guest 环境会扩大暴露范围：进程、脚本、调试输出和误操作都可能接触凭据。

对于由模型自主调用工具的 Agent，长期 key 一旦进入文件系统或环境变量，就进入了 Agent 可访问的执行边界；用户或其他上游输入可能直接要求读取，也可能通过 prompt 诱导 Agent 使用 bash、文件工具、日志或任务结果带出凭据。

![CubeEgress 凭据注入链路](./assets/2026-08-28-horizon-insights/03-cubeegress-credential.jpg)

*图 3：CubeEgress 凭据注入链路*

我们用 CubeEgress 在出口侧管理凭据。BWA 在创建 Sandbox 时只传递 Egress 所需的受控配置，长期凭据保存在出口侧。请求离开 Sandbox 时，CubeEgress 根据目标服务匹配策略并注入相应认证信息。Guest 中的代码按照普通 HTTP 或 SDK 方式调用服务，Workspace、prompt 和进程环境中不保存长期 key。

这套设计在凭据隔离之外还有几个直接价值：凭据保留在 guest 之外，由平台集中轮换或撤销，并限制允许访问的目标；Sandbox template 无需随 key 变化而重建；模型调用、MCP 工具和遥测出口可以采用一致的网络边界。

对金融场景来说，这几点都是必备需求，凭据的轮换周期、访问目标的约束、审计边界都需要由平台侧统一控制，而不是交给每个 Agent 实例自行管理。Host Mount 的只读分发和 CubeEgress 的出口注入一起，构成了 4as 沙箱化的两条数据面支柱：版本化资源按需可见，长期凭据按需注入。

## 五、选型 Cube Sandbox 的考量

我们当时对 CubeSandbox 的选型，主要考量了部署边界、接口兼容和运行时等方面能力，核心的评估如下：

- **部署边界方面**：金融机构经常要求应用运行在指定云环境、专有网络或客户侧基础设施中，E2B Cloud 等托管沙箱无法覆盖这类要求。CubeSandbox 可以部署在自有环境和客户侧节点中，两种形态保持相近的 API 和运行模型。

- **接口兼容方面**：BWA 的 Sandbox adapter 基于 E2B-compatible API，CubeSandbox 保留了这一接口模型，Sandbox 创建、文件操作、命令执行和动态访问可以沿用现有 SDK 习惯。本地部署与 API 兼容是两个独立维度——执行环境由团队或客户运营，上层 runtime 仍可使用成熟的 SDK 契约，减少了专有适配代码。该组合减少了 BWA 的专有适配代码，也为后续版本升级保留了清晰边界。

- **数据面完整性方面**：自建容器执行层需要自行补齐模板、实例生命周期、动态访问、共享资源挂载、出站代理和凭据管理。CubeSandbox 已提供这些基础组件，BWA 可以集中处理 Agent 产品语义。Host Mount 和 CubeEgress 对当前业务的作用尤其直接。前者承接版本化领域资源分发，后者承接模型与工具服务的凭据边界。这两项能力已经进入生产运行链路，选型价值能够落实到具体架构中。

- **维护响应方面**：CubeSandbox 维护团队对技术问题响应及时，版本更新频率较高，问题能够较快进入定位和沟通。

- **启动速度与开销方面**：当前任务以模型推理、数据访问、代码处理和文件生成组成，单次 run 的主要耗时通常来自模型与工具调用。CubeSandbox 的启动速度和运行开销能够满足现有任务需求。

## 六、Cube Sandbox 在生产化中的探索与优化经验

经过中心环境和客户侧项目的实际运行，生产化过程中的配套工作主要集中在生命周期和宿主数据面，其中有一些我们的实践和优化值得分享：

- **pause/resume 的 seccomp 兼容问题**：在启用沙箱自动 pause 时，曾出现 seccomp 兼容问题，当前相关环境关闭自动 pause，保留正常 idle 回收。`pause/resume` 作为 Cube 的密度优化特性，在金融场景的保守内核配置下需要单独评估，不能直接默认开启。

- **CoreDNS 重启影响 Egress 策略路由**：CoreDNS 或系统网络服务重启可能影响动态域名和 Egress 策略路由。CubeEgress 的出口注入依赖域名解析和策略路由的稳定性，DNS 层的抖动会直接传导到 Egress 链路。我们现已增加联动恢复与周期性校验，而不是依赖单次配置生效。

- **控制面健康不等于数据路径通畅**：控制面健康检查无法覆盖完整数据路径——CubeSandbox 控制面报 healthy，不代表 Sandbox 创建、文件写入、资源挂载、真实 Egress 这条链路都能跑通。我们的发布验收因此扩展为端到端验证：Sandbox 创建 → 文件写入 → 资源挂载 → 真实 Egress → 任务完成 → 回收，任何一环失败都视为发布未通过。

- **大版本升级需要重新核对宿主配置**：Cube Sandbox 大版本升级后，生命周期参数、Proxy 配置和自定义入口等宿主侧配置需要重新应用并核对。这些配置散布在宿主机的多个位置，升级脚本不会自动保留——目前需要人工对照清单逐项检查。

Cube Sandbox 将开源可部署、E2B-compatible API、Host Mount 和 CubeEgress 组合为一套完整的沙箱基础设施，使 4as 能够以同一套 Agent runtime 支撑客户侧部署和即将推出的 Cloud 版服务。投研 Agent 的长任务、版本化领域资源和受控外部访问，也展现了 CubeSandbox 在单次代码执行之外承载完整 Agent runtime 的能力。随着 4as 服务更多金融投资与研究机构，Cube Sandbox 将继续作为这套产品的核心执行底座。

*感谢弘则弥道王正凯老师，对以上内容的贡献。*
