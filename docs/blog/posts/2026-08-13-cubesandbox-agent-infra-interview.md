---
title: "From Sub-100ms Startup to Production-Grade Deployment: Why Tencent Cloud Rebuilt the Agent Sandbox"
date: 2026-08-13
author: InfoQ
description: "In early 2026, OpenClaw single-handedly sparked a local terminal Agent craze, and people began handing over file systems, browsers, email, terminals, and various account permissions to Agents. But behind the hype lies a risk more immediate than model hallucination. This article is InfoQ's exclusive interview with Jin Feng, head of Tencent Cloud's IaaS Frontier Technology Team, exploring Cube's evolution from Serverless infrastructure to Agent sandbox and the path from 'can run' to 'production-ready.'"
featured: false
---

# From Sub-100ms Startup to Production-Grade Deployment: Why Tencent Cloud Rebuilt the Agent Sandbox

> This article was first published by InfoQ as an exclusive interview with Jin Feng, head of Tencent Cloud's IaaS Frontier Technology Team, and is reproduced here with permission.

In early 2026, OpenClaw single-handedly sparked a local terminal Agent craze, and people began handing over file systems, browsers, email, terminals, and various account permissions to Agents. "Raising shrimp," as it were, became the trendiest thing in the first half of the year.

But behind the hype lies a risk more immediate than model hallucination.

Summer Yue, alignment lead at Meta's Superintelligence Lab, publicly shared that her "little crawfish" autonomously deleted and archived hundreds of her personal emails, completely ignoring her stop commands, and could only be terminated by manually shutting down the device. Concerns quickly extended from individual users to enterprises. Several tech companies began restricting employees from using "little crawfish" on work devices for security reasons; China's Ministry of Industry and Information Technology also publicly warned that misconfigured "little crawfish" instances could face cyber attacks and data breach risks.

OpenClaw pushed Agents into the spotlight, and also brought the layer of infrastructure beneath them — long overlooked — to the forefront. People began to realize that an Agent with system permissions but unpredictable behavior, running directly on a personal computer or production environment, is essentially "running naked." It needs an independent, isolated, recoverable execution environment — which is why sandboxes have recently drawn concentrated attention from developers and cloud providers alike.

Around this new layer of infrastructure, several explorations have emerged domestically and internationally. Anthropic released Claude Managed Agents in public beta, providing a complete managed Agent runtime platform; E2B packaged isolated sandboxes as a developer-callable service, becoming a de facto interface standard that many Agent projects actively support; in April 2026, Tencent Cloud officially open-sourced Cube Sandbox, positioning it as an execution environment foundation for AI Agents. (Cube Sandbox source: https://github.com/tencentcloud/CubeSandbox)

Among these explorations, Cube can be seen as a special sample for observing Agent Infra evolution. The reason is that it tackles a sufficiently new Agent Infra challenge, but its foundation is a set of infrastructure that evolved from a Serverless production system.

Unlike many sandbox projects that quickly appeared after the Agent boom, Cube's development dates back to around 2023, when it was solving底层 problems in Serverless scenarios. Subsequently, Cube entered code execution, data analysis, and Agent RL scenarios, and ultimately pivoted toward Agent Runtime. After open-sourcing, Cube quickly entered the overseas Agent ecosystem. In July, OpenClaw founder Peter Steinberger proactively submitted and merged a PR to integrate Cube into its provider system, alongside E2B, Modal, and other sandbox services.

From Serverless to Agent sandbox, which capabilities can be directly inherited, and which must be redesigned around Agents? What are Cube's design considerations? What is the real threshold for a sandbox to go from "can run" to "large-scale production-ready"? Why did Tencent Cloud choose to fully open-source this system? Around these questions, InfoQ recently interviewed Jin Feng, head of Tencent Cloud's IaaS Frontier Technology Team and a Tencent Level-14 R&D engineer, to understand Cube's journey from Serverless foundation to Agent sandbox, and the team's vision for next-generation Agent Infra.

## 1 "Agents Cannot Reuse Old Infra"

Agent requirements for infrastructure are undergoing a fundamental shift.

In the past, whether VMs, containers, or Serverless, the core problem infrastructure needed to solve was fairly clear: provide available computing resources and let applications run stably. But an Agent is not a traditional application. Driven by large models, it can autonomously plan, call external tools, access networks, and do anything a human can do. In the latest explorations, people expect Agents to autonomously execute long-horizon tasks lasting days or even weeks. Over such long lifecycles, how do you ensure the Agent's autonomous behavior remains controllable? Clearly, traditional computing resources can't solve this — a runtime environment matching Agent behavior is needed.

Jin Feng believes that Agents have distinctive characteristics, and the requirements can be broadly categorized into three types by scenario.

The first type is providing a fully isolated environment for Agent tool execution. This is currently the most common sandbox use case. The requirement is strong concurrent launch capability — as if calling a local function — and high resource utilization, enabling hundreds or even thousands of concurrent instances on a single server.

The second type is long-running tasks carried by Agent Harnesses. An Agent Harness is essentially a stateful service. During its long runtime, it continuously produces various intermediate and persistent state artifacts. The sandbox hosting the Agent Harness needs fast state save/restore capabilities to match users' demands for quick Agent pause/resume and clone/rollback.

The third type goes further: providing a unified foundation for services that Agents access. Unlike traditional services that emphasize steady-state operations, Agent-facing services may directly become part of the Agent training and inference loop, demanding higher requirements for rapid start/stop, branch exploration, and rollback. Additionally, how to make traditional services Agent-friendly with zero code changes is also a problem Infra needs to solve.

Faced with these new demand-level changes, continuing to use traditional infrastructure is clearly insufficient. "Agents do need a better, more advanced Infra, rather than continuing to reuse old-style Infra." Jin Feng believes that from the perspective of providing computing resources, VMs, containers, and Serverless can all run Agents, but they each only solve part of the problem.

For example, traditional VMs provide strong security isolation, but startup may take several seconds. When you add control plane scheduling, resource allocation, and network preparation, the end-to-end time from API request to a truly usable instance can be 5-10 seconds. In contrast, CubeSandbox's cold start is under 60ms, making it more suitable for high-frequency tool calls and bursty elastic scaling.

Docker containers start fast and have high resource utilization, making them a transitional choice for many Agent applications today. But their hard limitation is shared host kernel, making security isolation inherently weak. As Agent permissions expand and tasks become more critical, container security isolation issues will become increasingly prominent.

Serverless functions are closer to Agent needs in terms of rapid elasticity and pay-per-use billing, and are suitable for short, stateless tool tasks. But they are typically designed around event-driven, stateless services, using horizontal autoscaling for resource elasticity and scaling to zero when idle — making them a poor match for Agent's stateful execution model.

Sandboxes are drawing attention because they try to fill the gap between these traditional solutions: providing isolation boundaries approaching VMs, with startup speed and resource density approaching containers, while redesigning execution around Agent state saving, pause/resume, cloning, and rollback.

The real challenge isn't just making a sandbox — it's making it production infrastructure that truly enters enterprise environments.

"Enterprises may prioritize stability over performance." Jin Feng says Agent development is moving so fast that the underlying infrastructure hasn't formed mature best practices, and many teams are still using containers as a transitional solution. For stateful services like Agents, beyond whether the instance itself can run stably, whether task state, files, and execution environment can be fully recovered after failure is equally critical. Additionally, enterprises evaluate whether a project can be maintained long-term and whether it provides sufficient autonomy and control after deployment. This is why Cube's evolution always targets production-grade large-scale availability: not just making the sandbox run, but proving it can run stably and at scale in production.

## 2 From Serverless to Agent Sandbox: How Does Cube Cross the Production Threshold?

### Cube Had Been Running for Two-Plus Years Before Agents Went Viral

As mentioned earlier, Cube's foundation is a set of infrastructure that evolved from a Serverless production system, starting around 2023. At that time, the team was primarily working on Serverless scenarios. The ideal Serverless model is: when a function is called, the compute environment appears quickly; when the task ends, resources are released immediately. But at the time, many Serverless products only offered Lambda-like interfaces while relying on traditional technology underneath.

Tencent Cloud wanted to build a set of infrastructure that strictly matched this model — which is how Cube was born. Cube's original design goal was to build a system natively suited for fine-grained resources, extreme cold-start speed, and massive concurrency: when not called, it consumes almost no resources; when needed, it creates an environment in under a hundred milliseconds; after execution, it's quickly destroyed.

This starting point unexpectedly became the technical foreshadowing for Cube's entry into the Agent era.

In terms of technical architecture, Cube uses RustVMM+KVM. "We wanted to build lightweight infrastructure. At the time, building lightweight VMs based on RustVMM was a technology route that drew considerable industry attention. However, our choice differed from the common industry approach. Many projects chose Firecracker, but we chose Cloud Hypervisor." Jin Feng says Tencent Cloud faces more complex internal scenarios with many hardware-related requirements. Compared to Firecracker, Cloud Hypervisor natively supports more capabilities like device hot-plugging and hardware passthrough. The team chose to subtract from a feature-complete VMM, optimizing overall overhead to approach Firecracker's level.

On top of this technical route, Cube built three core capabilities.

The first is fast startup. Cube uses snapshot-based startup: pre-creating template snapshots and, upon request, directly restoring the runtime from a snapshot rather than going through a complete VM boot process, compressing resource launch time to under a hundred milliseconds.

The second is high concurrency. In Serverless scenarios, resources are frequently created and destroyed, demanding higher concurrency from both single-machine and cluster control planes. Inside compute nodes, Cube has done extensive async design and composed the network, storage, and other resources needed for sandboxes. At the cluster level, Cube didn't directly reuse traditional VM or Kubernetes control plane designs, instead making the control plane horizontally scalable: each compute node independently handles sandbox creation, and adding nodes synchronously increases the cluster's overall concurrent processing capacity.

The third is high density. To let a single machine host more instances, Cube introduced extensive resource sharing and copy-on-write mechanisms. Different instances can share the same read-only kernel, root filesystem, and other underlying resources; only when an instance actually writes data is independent resource allocated, reducing大量 duplicate memory and storage overhead, enabling a single node to host thousands of lightweight instances.

These capabilities were originally built around Serverless characteristics, but as Agents emerged, the team found they equally matched Agent requirements for execution environments. This is the technical foundation for Cube's transition from Serverless infrastructure to Agent sandbox.

"The whole process can be roughly divided into three phases." Jin Feng says the first phase centered on code execution and data analysis. Early on, products like E2B and Manus began letting Agents execute code in isolated environments or read Excel files to complete data analysis and report generation. Cube initially targeted these two relatively clear scenarios and gradually entered products like Tencent Yuanbao.

The second phase centered on Agent Reinforcement Learning (Agent RL). Compared to simple code execution, Agent RL simultaneously launches large numbers of training environments, demanding more from image management, fast startup, and high concurrency. Cube's architecture capabilities accumulated during the Serverless phase were further validated in this phase. Jin Feng says Cube's metrics in MiniMax scenarios significantly outperformed other solutions, gradually building industry reputation.

The third phase moved from serving model training to Agent Runtime. As OpenClaw and similar products drove the rise of local terminal Agents, people realized that traditional infrastructure had become a "make-do" — Agents should have execution environments better suited to their own characteristics.

"We didn't start building this system after Agents went viral." Jin Feng notes that Cube's underlying capabilities have been through two-plus years of production refinement in Serverless and Tencent internal businesses, and the main architecture is not a freshly completed prototype. This is the most fundamental difference from many newly-appeared Agent sandboxes: it first had a foundation designed for high-concurrency, short-lifecycle workloads, then gradually changed system boundaries based on Agent behavior.

### From "Running Fast" to "Managing Well": Cube Begins Adapting the Foundation for Agents

Therefore, in v0.3.0, the team first added snapshot, clone, and rollback capabilities to Cube, addressing Agent's need for stateful environment replication and recovery. Snapshot can save a running sandbox's memory, runtime state, and disk as an independent snapshot — usable even after the source sandbox is destroyed. Clone can split one sandbox into N. Rollback lets a sandbox restore in-place to a previous snapshot's state, fully recovering memory state and filesystem. For Agents, this is equivalent to simultaneously gaining "clones" and "time travel" for their environments.

After solving state replication and recovery, Cube's next step was handling the risks of Agent behavior itself. "Agents are driven by large models — we can't predict what they'll do. You can put them in a sandbox, but what they do inside isn't entirely knowable." Jin Feng says. So in v0.4.0, Cube focused on adding egress governance, credential hosting, and network observability and auditing. These additions were essentially about adding boundaries to Agent unpredictability without weakening Agent flexibility.

By v0.5.0, Cube wanted to solve "stable, efficient, broad." The core feature was teaching sandboxes to auto-pause (AutoPause) and auto-resume (AutoResume), adding native ARM support, and moving from single-machine demos to cluster deployment, providing a more complete deployment architecture for production and lowering the barrier for enterprises to introduce Cube into real business.

"We don't want to interfere with Agent flexibility, because generalization is the value of AI; but the uncontrollable parts still need boundaries." Jin Feng says. From v0.3.0's snapshot/clone/rollback, to v0.4.0's egress governance/credential hosting/network observability, to v0.5.0's auto pause/resume/ARM support/production deployment, Cube's version evolution preserved Agent autonomy and generalization while constraining its uncertainty within controllable bounds.

But for enterprises, this isn't enough. Whether new infrastructure can truly enter production depends on whether it can be deployed, operated, and integrated with existing systems.

### Truly Embedded in Enterprise Infrastructure

Cube initially ran primarily on physical machines. While this fully leveraged KVM and lightweight virtualization performance, it also raised the barrier for external users. Especially in cloud environments, requiring enterprises to provision dedicated physical machines adds cost and operational complexity. So shortly after open-sourcing, the team gradually added the ability to run Cube on cloud VMs. The latest v0.6.0 adds Kubernetes support, continuing the trend of lowering deployment barriers.

"Kubernetes is the de facto standard for many enterprises' infrastructure; many companies' machine resources are already managed by Kubernetes." Jin Feng says the team using Agents and the team managing clusters are often different people. If deploying Cube requires carving out machines from an existing Kubernetes cluster and building a separate one, business teams can't easily drive adoption.

Starting from v0.6.0, Cube's control plane components and compute nodes can be deployed directly to Tencent Cloud TKE, standard Kubernetes, or k3s via Helm Chart. Enterprises no longer need to maintain a separate deployment system outside their existing infrastructure — Cube components become standard workloads under Kubernetes management, reusing mature deployment, upgrade, autoscaling, and operations capabilities. In subsequent versions, Cube plans to make Kubernetes deployment more "native": moving from Helm to CRD- and Operator-based native management, with smooth upgrade capabilities.

Another v0.6.0 capability drawing developer attention is the E2B-compatible Volume framework. "Volume is a capability many developers care about because Agents typically need persistent storage during execution — not all data can be destroyed with the sandbox after a task ends. An Agent may need to load multiple Skills or produce new Skills during execution; in data analysis scenarios, it may need to read external Excel files and output new ones. So sandboxes need persistent storage decoupled from their own lifecycle."

Jin Feng mentions that in early versions, Cube provided a relatively interim solution: binding host directories into sandboxes. Many external users used this capability to meet persistent storage needs. With Volume, Cube further abstracted storage. Beyond Sandbox, the system added a parallel Volume abstraction representing persistent storage. The team also referenced Kubernetes CSI's design approach, using a plugin model — users can write CSI-like plugins for different storage backends while maintaining a unified interface.

Kubernetes support addresses how Cube enters enterprises' existing compute and operations systems; Volume addresses how Agent files and task artifacts enter enterprises' existing storage systems; and CubeEgress, released in earlier versions, provides enterprises with comprehensive network governance. These updates together accelerate Cube's entry into enterprise infrastructure systems, representing Cube's evolution from a sandbox providing isolated execution to an Agent runtime foundation embeddable in enterprise infrastructure.

## 3 Open Source and Openness: Pushing Agent Infra Forward

In April 2026, Tencent Cloud officially open-sourced Cube. Jin Feng admits that the more direct reason for open-sourcing was that Agent development had outpaced infrastructure evolution. Even today, the industry still hasn't formed a clear answer on what kind of runtime environment Agents truly need. A common view is that enterprises already have Kubernetes and containers, so there's no need to introduce a new sandbox system. After all, from the perspective of "getting programs running," existing infrastructure can indeed do the job.

But many differences only become apparent after actual use. Launching an isolated environment in sub-100ms, cloning multiple execution branches at once, rolling back state after an Agent mistakenly deletes files, releasing resources when idle and seamlessly resuming — these are not problems traditional containers were originally designed to solve.

"We chose to open-source Cube to let more people experience it firsthand and see what sandboxes can do that traditional solutions struggle with. Only when developers actually deploy and use it can they more intuitively understand what new requirements Agents impose on runtime environments." Jin Feng says.

Data shows that after open-sourcing, Cube surpassed 4,000 GitHub Stars in just 4 days; 3 months later, Star count exceeded 10,000. Such rapid growth at least demonstrates that Agent execution environments have become a concern developers普遍 care about. Jin Feng judges that as industry attention to Agents continues to rise, Agent-related infrastructure will also receive more attention.

For future version evolution, the Cube team has already made plans. Currently, Cube's snapshot, recovery, and state management capabilities are mostly built at the single-machine level. The team's next important direction is to elevate the sandbox abstraction from single-machine to cluster, enabling sandboxes to migrate and recover across nodes within a cluster. When a node fails, sandboxes running on it can quickly recover on other nodes, further improving the sandbox's own high availability.

Once sandboxes migrate across nodes, the first problem to solve is storage. In single-machine environments, sandboxes can rely on local disk; at cluster level, compute and state must be decoupled, so sandboxes can re-mount original data when recovering on any node. The Volume framework introduced in v0.6.0 is the starting point for this path. Subsequently, the team needs to continue adapting distributed storage while maintaining startup speed and I/O performance across different enterprise environments.

Another direction to fill is observability. Currently, Cube can already observe which external services a sandbox accesses at the network layer, performing traffic auditing and access control. But Agent behavior isn't limited to the network. It also calls system commands, modifies files, starts processes, and even executes high-risk operations. The team wants to push observability further down to the OS layer, more completely reconstructing what the Agent did inside the sandbox. When it executes sensitive operations, the system should have the opportunity to audit or even block in real time.

"Today we call it Sandbox, but it has far exceeded a sandbox. It represents an Infra system." Jin Feng says.

## 4 Conclusion: When Infra Begins Redesigning for Agents

When Agents were just chat-window assistants, risk was far from ordinary people. But once they gained file system, terminal, email, and production system permissions, the problem becomes concrete: where does it execute, what can it access, how to recover after mistakes, and who records and constrains its behavior?

Sandboxes thus became the earliest visible part of Agent infrastructure, but certainly not the last. For the future direction of Agent Infra evolution, Jin Feng judges that at least three changes are worth watching.

The first change is Agents moving from individual units to Agent Teams. Today's infrastructure tends to treat each Agent as an individual, isolating them through sandboxes; but as multiple Agents begin to divide labor and collaborate, infrastructure needs to provide shared context, task artifact exchange, and collaborative execution spaces.

Relevant practices have already emerged in the community: designing Sandbox and Volume as independent abstractions, then using Volume to划分 team shared spaces and individual private spaces, letting different Agents share files and task results while maintaining isolation. In the future, if using storage as an intermediary can't meet collaboration efficiency needs, Agents may develop more direct communication requirements.

The second change is that the primary users of more and more services will shift from humans to Agents. "Today's services are designed for humans — emphasizing human-visible, human-understandable interfaces, but these may be inefficient for Agents and models. Moreover, when the user shifts from human to Agent, the service itself faces more burstiness and branch-experiment challenges."

This means that "providing services for Agents" won't just be building a separate entry point or website. Real change will penetrate deep into services — system interfaces, throughput, permission control, and interaction patterns may all need to adjust accordingly.

The third change is that Agent risk boundaries will gradually extend beyond the sandbox. Today, discussions about Agent unpredictability typically limit risk to an isolated environment; but as Agents begin operating databases, calling enterprise services, and modifying production systems, their "tentacles" will reach more systems, and the potential failure domain will expand accordingly.

"After the Agent's tentacles extend beyond the sandbox, how do we still manage it?" Jin Feng believes the future problem to solve is no longer just how to confine an Agent in a safe environment, but how to continuously observe, audit, and constrain its behavior as it executes tasks across multiple systems, while preserving Agent autonomy and generalization, keeping the entire process within human-controllable bounds.

From Agent Teams collaboration, to redesigning services for Agents, to extending security boundaries across the entire production system — the sandbox is just the starting point of this infrastructure reconstruction. As Agents gradually transform from tools into primary actors in the digital world, Infra can no longer停留在 "traditional systems barely running" but must truly begin redesigning around Agent behavior.

## Related Links

- Cube Sandbox source: https://github.com/tencentcloud/CubeSandbox
- Documentation: https://cubesandbox.com/zh/guide/introduction.html
- Cube Sandbox system design considerations: https://xie.infoq.cn/article/510579436d9f297700292cac4
