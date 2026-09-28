---
title: "Sandboxing Financial Research Agents: Hongze Info's Architecture and Selection Practice with CubeSandbox"
date: 2026-08-26
author: Wang Zhengkai (CTO, Hongze Midao)
description: "A single financial research Agent run can last tens of seconds to several minutes, reading research methodologies and data definitions, calling data APIs, running scripts, generating intermediate files, and having the model synthesize results. The sandbox carrying it can't just 'run a snippet of code' — it must contain the complete Agent loop in an isolated environment. This article introduces three core use cases of CubeSandbox in Hongze's 4as Agent."
featured: false
---

# Sandboxing Financial Research Agents: Hongze Info's Architecture and Selection Practice with CubeSandbox

By｜CTO, Hongze Midao · Wang Zhengkai

**Editor's note:** A single financial research Agent run can last tens of seconds to several minutes, reading research methodologies and data definitions, calling data APIs, running Python or Node scripts, generating intermediate files, and having the model synthesize results. The actions, files, and external services encountered during execution cannot be fully enumerated at publish time. This means the sandbox carrying it can't just "run a snippet of code" — it must contain the complete Agent loop in an isolated environment.

This article introduces three core use cases of CubeSandbox in Hongze's 4as Agent: the complete Pi loop running inside the Sandbox, DAS shared resources distributed via Host Mount, and model/tool credentials managed at the egress side via CubeEgress. It then covers the practical experience with localized deployment, API compatibility, performance, and production operations.

## 1. Business Context: The Controlled Execution Environment for Research Agents

Hongze Info is an AI innovation team under Hongze Research, serving various financial investment and research institutions. The team's 4as (All (Domain) Agents as a Service) is a financial research Agent product that organizes domain knowledge, research methodologies, data tools, and persistent state into deployable research Agents. 4as has been deployed in multiple commercial projects, and a Cloud version for more institutional users will launch soon.

Research Agents in 4as need to perform data reading, data processing, code execution, file generation, external tool calls, and multi-round model interactions. A single task typically lasts tens of seconds to several minutes, touching user workspaces and external services, with specific actions that cannot be fully enumerated at publish time. Therefore, the system needs to handle the following constraints:

- Each Agent run has independent process and file boundaries;
- Domain resources are mounted by user authorization and version;
- User private files are stored separately from platform shared resources;
- Sandboxes can be reclaimed, while business state and research results persist;
- External model and data services are accessible, but long-term credentials never enter the guest environment;
- The same Agent runtime can run in both central and customer-side deployments.

CubeSandbox provides the unified sandbox execution foundation for 4as. It creates an independent execution environment for each Agent run, handling runtime templates, file and command execution, domain resource mounting, controlled egress, and Sandbox lifecycle management. Additionally, BWA (Bounded Workspace Agent) is 4as's controlled runtime, organizing the permissions, state, workspace, and interaction protocols needed by the product on top of CubeSandbox's base capabilities.

## 2. Technical Architecture: Pi Loop Running Inside CubeSandbox

4as's core Agent execution is based on the Pi coding agent SDK open-sourced by Mario Zechner. The team calls the complete model-tool main loop the "Pi loop," and adds scope, Workspace, task lifecycle, event protocol, and Sandbox adapter on top of the Pi SDK.

In the production path, the Pi SDK and complete main loop both run inside the CubeSandbox guest. One loop includes model round-trips driven by `session.prompt()`, tool selection and execution, bash and file operations, MCP tool calls, event streams, and session state progression. CubeSandbox provides the same isolated process, file, and network environment for all these actions.

The current execution chain consists of four layers: Host provides the user-facing product interface and backend APIs, completing identity verification before forwarding requests and scope to BWA Runtime. BWA verifies tenant, user, DAS, and thread, creates or reuses CubeSandbox instances via E2B-compatible API, and manages run state, cancellation, timeout, workspace paths, and persistent checkpoints. CubeSandbox carries the execution plane, with Pi runner entering the guest via Sandbox template, handling the specific model and tool loop.

![4as Research Agent execution chain](./assets/2026-08-28-horizon-insights/01-agent-execution-flow.jpg)

*Figure 1: 4as Research Agent execution chain*

Tokens, tool events, state, and file changes produced by the Guest are first returned to BWA via an internal execution protocol, which BWA then converts to its own stable SSE events for the frontend. The frontend only depends on the BWA contract and is unaware of CubeSandbox's creation API, dynamic hostname, or internal runner protocol — the three layers can evolve independently. The core of this separation is decoupling the execution lifecycle from the business lifecycle: CubeSandbox carries the reclaimable compute process, while BWA and its persistent storage retain user workspaces, research outputs, and thread state. After sandbox destruction, business state can still be recovered in the next run.

## 3. Host Mount: Distributing Domain Resources by Version

DAS (Domain Analyst Service) is the domain analyst service in 4as. Its shared definition includes domain knowledge, research methodologies, data references, assets, deterministic computation code, and Function definitions. The difference between DAS and a regular Skill: a Skill describes the execution method for a type of action, while DAS describes a continuously running domain analyst — a distributable domain definition + managed state + state transition mechanism.

The shared definition of DAS needs to be distributed to each execution environment. If every run uses the file API to transfer large domain resources into the sandbox, the overhead is high. We use CubeSandbox's Host Mount to solve this. Host Mount is responsible for distributing the shared, versioned, read-only portions. The distribution chain is roughly:

![DAS distribution chain](./assets/2026-08-28-horizon-insights/02-das-distribution.jpg)

*Figure 2: DAS distribution chain*

Several key constraints of Host Mount form the foundation of the distribution mechanism: each revision corresponds to an immutable file tree, BWA only generates mount entries for DAS authorized for the current run, readOnly is fixed to true, host paths are located within a restricted shared directory, and path whitelist and read-only attributes together constrain the mount scope. This means Pi can consume these resources like local files, but the running Agent cannot modify published versions in place — domain capability upgrades can only be done through FCP publishing a new revision. The mutable parts of DAS take a different path: user private workspaces, instance current state, and state transition results are persisted by the runtime, not written back to the shared mount directory. Runtime state and shared definitions thus form two types of independent ownership — the former belongs to specific instances, the latter to distributable versions.

This distribution approach solves several practical problems:

- Large domain resources don't need to be repeatedly transferred via file API for each run;
- Multiple Sandboxes can consume the same host cache, with file content fixed by revision;
- Authorization results are directly reflected in the mount list — unmounted domain resources are invisible to the guest;
- Shared definitions remain read-only, with Agent outputs going to private workspaces;
- Central and customer-side environments use the same revision and directory semantics.

Host Mount thus becomes part of the DAS distribution chain. It connects 4as content and the version control plane FCP's publishing process with actual execution inside CubeSandbox, while maintaining boundaries between shared definitions, instance state, and user files.

## 4. CubeEgress: Managing Credentials at the Egress Side

Pi inside the Sandbox needs to access model services, MCP services, and some data APIs. Writing long-term API keys into the guest environment expands the exposure surface: processes, scripts, debug output, and misoperations can all touch credentials.

For an Agent that autonomously calls tools via a model, once a long-term key enters the filesystem or environment variables, it's within the Agent's accessible execution boundary; users or other upstream inputs may directly request to read it, or may induce the Agent via prompt to use bash, file tools, logs, or task results to extract credentials.

![CubeEgress credential injection chain](./assets/2026-08-28-horizon-insights/03-cubeegress-credential.jpg)

*Figure 3: CubeEgress credential injection chain*

We use CubeEgress to manage credentials at the egress side. When creating a Sandbox, BWA only passes the controlled configuration needed by Egress — long-term credentials stay on the egress side. When a request leaves the Sandbox, CubeEgress matches the target service policy and injects the corresponding authentication information. Code in the Guest calls services using normal HTTP or SDK patterns, with no long-term keys in the Workspace, prompt, or process environment.

Beyond credential isolation, this design has several direct values: credentials remain outside the guest, managed centrally by the platform for rotation or revocation, with access targets restricted; Sandbox templates don't need rebuilding when keys change; model calls, MCP tools, and telemetry egress can use a consistent network boundary.

For financial scenarios, these are all essential requirements — credential rotation cycles, access target constraints, and audit boundaries all need platform-level unified control, not delegated to each Agent instance. Host Mount's read-only distribution and CubeEgress's egress injection together form the two data-plane pillars of 4as sandboxization: versioned resources visible on demand, long-term credentials injected on demand.

## 5. Considerations in Selecting CubeSandbox

Our selection of CubeSandbox primarily considered deployment boundaries, API compatibility, and runtime capabilities. The core evaluation:

- **Deployment boundaries**: Financial institutions often require applications to run in designated cloud environments, private networks, or customer-side infrastructure. Managed sandboxes like E2B Cloud cannot cover these requirements. CubeSandbox can be deployed in self-owned environments and customer-side nodes, with both forms maintaining similar APIs and runtime models.

- **API compatibility**: BWA's Sandbox adapter is based on E2B-compatible API, and CubeSandbox preserves this interface model. Sandbox creation, file operations, command execution, and dynamic access can use existing SDK conventions. Local deployment and API compatibility are two independent dimensions — the execution environment is operated by the team or customer, while the upper-layer runtime still uses mature SDK contracts, reducing proprietary adaptation code. This combination also preserves clear boundaries for future version upgrades.

- **Data-plane completeness**: Self-built container execution layers need to implement templates, instance lifecycle, dynamic access, shared resource mounting, egress proxy, and credential management independently. CubeSandbox already provides these base components, so BWA can focus on Agent product semantics. Host Mount and CubeEgress are especially directly useful for current business. The former handles versioned domain resource distribution, the latter handles credential boundaries for model and tool services. Both capabilities have entered the production runtime chain — the selection value materializes in the concrete architecture.

- **Maintenance responsiveness**: The CubeSandbox maintenance team responds promptly to technical issues, with relatively frequent version updates, and issues quickly enter investigation and communication.

- **Startup speed and overhead**: Current tasks consist of model inference, data access, code processing, and file generation. The main time consumption for a single run usually comes from model and tool calls. CubeSandbox's startup speed and runtime overhead meet current task requirements.

## 6. CubeSandbox Exploration and Optimization Experience in Productionization

After actual operation in central environments and customer-side projects, the supporting work during productionization mainly focuses on lifecycle and host data plane. Some practices and optimizations worth sharing:

- **seccomp compatibility issue with pause/resume**: When enabling sandbox auto-pause, a seccomp compatibility issue occurred. The affected environment currently has auto-pause disabled, retaining normal idle reclamation. As a density optimization feature of Cube, `pause/resume` needs separate evaluation under the conservative kernel configurations of financial scenarios — it cannot be enabled by default.

- **CoreDNS restart affecting Egress policy routing**: CoreDNS or system network service restarts can affect dynamic domain names and Egress policy routing. CubeEgress's egress injection depends on the stability of DNS resolution and policy routing — DNS-layer jitter directly propagates to the Egress chain. We've added linkage recovery and periodic validation, rather than relying on a single configuration taking effect.

- **Control plane healthy ≠ data path clear**: Control plane health checks cannot cover the complete data path — CubeSandbox control plane reporting healthy doesn't mean the full chain of Sandbox creation → file writes → resource mounting → real Egress is working. Our release verification has expanded to end-to-end validation: Sandbox creation → file writes → resource mounting → real Egress → task completion → reclamation. Any link failure is treated as release failure.

- **Major version upgrades require re-verifying host configuration**: After CubeSandbox major version upgrades, lifecycle parameters, Proxy configuration, and custom entry points need to be re-applied and verified. These configurations are scattered across multiple host locations, and upgrade scripts don't automatically preserve them — currently requiring manual checklist verification.

CubeSandbox combines open-source deployability, E2B-compatible API, Host Mount, and CubeEgress into a complete sandbox infrastructure, enabling 4as to use the same Agent runtime for both customer-side deployments and the upcoming Cloud service. The long tasks, versioned domain resources, and controlled external access of research Agents demonstrate CubeSandbox's ability to carry a complete Agent runtime beyond single code execution. As 4as serves more financial investment and research institutions, CubeSandbox will continue as the core execution foundation for this product.

*Thanks to Wang Zhengkai of Hongze Midao for contributing to this content.*
