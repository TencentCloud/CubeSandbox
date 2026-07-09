---
title: "Cube Sandbox v0.4.0: From Isolating Agents to Governing Agents"
date: 2026-06-17
author: Cube Sandbox Team
description: "Following v0.3.0's snapshot/clone/rollback trio, v0.4.0 ships 58 commits from 15 contributors centered on three questions: egress governance (CubeEgress L7 proxy + credential injection + domain filtering + access audit), observability (container log forwarding via vsock), and consistency (node component version matrix + template compatibility checks). Also delivers a 41% reduction in network P99 latency and cuts template build peak disk from 4.2x to 1.2x image size."
featured: true
weight: 2
---

# Cube Sandbox v0.4.0: From "Isolating Agents" to "Governing Agents"

Two weeks after v0.3.0 shipped the snapshot / clone / rollback trio, v0.4.0 is here. The changelist is broad, but if you had to summarize it in one sentence: **Cube is evolving from a "high-performance isolation runtime" into a "governable Agent infrastructure."**

Once isolation is taken to its logical extreme, the next question isn't just "how do we lock the Agent in" — it becomes:

- **Egress governance** — What outbound traffic can an Agent send from the sandbox?
- **Observability** — Can the outside world see what happens inside the sandbox?
- **Consistency** — Across dozens of machines in a cluster, are they really running the same versions?

v0.4.0 tackles all three.

## 1. Domain Filtering

Cube's networking model has supported AllowOut / DenyOut IP/CIDR filtering since the first release, enabling fine-grained outbound traffic control at the IP level. But in real production scenarios, outbound access typically uses domain names resolved via DNS — the IP addresses aren't known at policy configuration time, and they can change at any moment. This model works for prototyping, but falls short in production.

Domain-based filtering strengthens outbound traffic control and better matches how Agents actually use sandboxes. Configuring one or more domains in AllowOut means allowing traffic to the IPs those domains resolve to. AllowOut supports mixing domains and IP/CIDR entries. Domains support both exact match and suffix wildcard patterns — `*.qq.com` allows any subdomain ending in `.qq.com`. DenyOut does not support domains; when AllowOut contains at least one domain, DenyOut must include `0.0.0.0/0` to deny any traffic to non-allowlisted domains.

## 2. CubeEgress: The Sandbox's First L7 Egress Gateway

Whether it's the IP/CIDR filtering available since v0.1 or the domain filtering above, both control outbound traffic at the IP level — similar to using NetworkPolicy for L3/L4 isolation. But the Agent's world no longer operates at the L3/L4 abstraction. An Agent calls `https://api.openai.com/v1/chat/completions` — it's a combination of host + path + method. NetworkPolicy cannot see this layer.

CubeEgress is a brand-new OpenResty-based egress gateway that intercepts sandbox outbound traffic via TPROXY and enforces L7 policies before requests leave the cluster. It consists of 9 Lua modules running on OpenResty/nginx (~2,200 lines of code) plus Go-side integration — CubeAPI (provides API for configuring sandbox egress policies), CubeMaster (generates and bakes CA certificates into templates), NetworkAgent (policy distribution), and CubeEgress (policy enforcement). SNI and host configured in rules policies are automatically allowlisted in AllowOut, and the corresponding 80/443 HTTP/HTTPS traffic is redirected from CubeVS to CubeEgress for processing.

**CubeEgress's design puts the control point at L7.** It works by transparently and selectively intercepting outbound traffic via TPROXY — code inside the sandbox is completely unaware. Requests go out as usual, but must pass through CubeEgress before leaving the cluster.

**The policy matching dimensions are five: SNI, host, method, path, scheme. The actions are four: allow, deny, audit, inject.** It also supports writing Lua scripts to easily extend any functionality you need.

### 2.1 L7 Application-Layer Filtering

IP/CIDR/domain filtering operates at the destination IP level. CubeEgress supports five-tuple filtering on SNI, host, method, path, and scheme. SNI and host support suffix wildcards, path supports prefix wildcards. Each element is optional, but at least one of SNI/host must be configured to steer traffic to CubeEgress.

Each sandbox can be configured with multiple rules, each with five-tuple filtering, matched in order. Each rule can allow or deny to implement fine-grained filtering policies.

### 2.2 HTTPS Proxying

When CubeEgress processes HTTPS traffic from sandboxes, it needs to decrypt the traffic. CubeEgress supports a self-distributed certificate service — when a sandbox makes an HTTPS request, it receives a certificate from CubeEgress, completes encryption/decryption with CubeEgress, which then interacts with the original destination service via HTTPS.

When building templates, Cube automatically bakes CubeEgress's root certificate into the sandbox image so that certificate validation succeeds. This entire process is closed-loop within Cube.

### 2.3 Credential Injection: Agent Code Never Touches Your API Key Again

In the traditional approach, when an Agent inside a sandbox needs to call OpenAI, `OPENAI_API_KEY` is typically injected as an environment variable. This means: the Agent's Python code can read it, any third-party library it imports can read it, it may appear in the LLM context, and it may end up in logs. Any single point of failure, and the key is out.

CubeEgress flips this — **the key only exists at the proxy layer and never enters the sandbox.**

You configure `EgressRule.inject` to specify "for any request to `api.openai.com`, automatically append `Authorization: Bearer ${KEY}`." Agent code inside the sandbox just sends bare requests or carries any arbitrary key — the real key is attached to the header by CubeEgress at forwarding time. From code to logs, the Agent never touches the original key. In CubeEgress's own security audit logs, credential fields are also replaced with `***REDACTED***`, preventing audit logs from becoming an exfiltration channel.

### 2.4 Auditing: Every Outbound Request Leaves a Traceable Structured Record

Every request passing through CubeEgress generates a structured JSON log — which domain was accessed, what method was used, what headers were present, what the body contained. Combined with the redactor Lua module for request body desensitization, you can retroactively trace what the Agent accessed without writing user PII into logs.

## 3. Container Log Forwarding: Sandbox Output Is Now Visible to the Host

Previously, stdout/stderr output from container processes stayed inside the sandbox — invisible to the host. Debugging required `cubecli exec` to dig around; production log aggregation was impossible.

The difficulty is that sandboxes are hardware-isolated MicroVMs, not ordinary containers. stdout/stderr can't be read from the host process's fd like `docker logs`. You must capture inside the sandbox, transport across the VM boundary, and persist on the host side — without using the business network, or logs and application traffic get mixed together.

v0.4.0's solution is a **dedicated vsock channel**:

- **Sandbox agent side**: Detects the need to open a log channel via the `cube.container.log_forwarding=true` annotation injected by the shim. Creates pipes for the init process's stdout/stderr with 1 MiB buffer, `O_NONBLOCK` — these two parameters are critical, **ensuring that slow log writes never block the Agent process**.
- **Transport**: Logs travel over a dedicated vsock connection (physically isolated from the control plane and business network), consumed by the shim on the host side.
- **Viewing**: Sandbox logs: `cubecli logs ${sandboxid}`; template logs: `cubecli logs --tpl ${templateid}`.
- **Exit sequencing**: The write-end FD is proactively closed on process exit to ensure the read-end receives EOF. This is a seemingly trivial but easily-missed detail — without it, the last few log lines might never arrive.

The companion `cubecli cubebox logs` subcommand supports `--tail`/`--head`/`--all`/`--stderr`.

## 4. Node Component Version Matrix: Making "Version Drift" Visible

You upgraded CubeMaster but forgot a node's Cubelet; you refreshed the guest-image but two nodes' kernels didn't follow. This inconsistency won't explode immediately, but one day it will surface in a completely unexpected way — a sandbox runs fine on node A but fails on node B, and after investigation you discover a component is two minor versions behind.

It took Kubernetes years to reach consensus on this: **version visibility is the zero-th problem of cluster operations.** v0.4.0 builds this infrastructure layer in three steps:

### 4.1 Step One: Unified Build-Time Version Metadata Injection

All Go binaries use ldflags, all Rust binaries use build.rs to inject the version / commit / build-time triple at compile time. The one-click deployment script generates `release-manifest.json`. `cubecli version` and `cubemastercli version` produce identical output formats.

Sounds simple, but anyone who has worked on polyglot projects knows — **making Go and Rust build chains produce consistent, same-source version information requires deliberate constraint.** Before this, "same component, different versions depending on which tool you ask" was a reality Cube also faced.

### 4.2 Step Two: Automatic Node Reporting, CubeMaster Maintains the Matrix

Cubelet automatically collects guest-image, cube-agent, kernel, and control plane component versions on each node and reports them to CubeMaster. CubeMaster maintains the cluster-wide version matrix in the `node_component_version` table (migration 0004), grouping by component and reporting same-version nodes — **version drift is exposed as set differences**, making it immediately obvious which node diverges from the majority.

CubeAPI provides both summary and detail endpoints, and the Web UI adds a `Versions.tsx` page for visualization.

### 4.3 Step Three: Template-Component Version "Reconciliation"

This goes a step further. Cube introduces the `template_versions` table (migration 0006) to record the component versions each template copy depends on, and provides two endpoints:

- `/templates/compat`: Cluster-wide template compatibility view
- `/templates/compat/{id}`: Single template detail

The Web UI introduces five components — CompatBadge, CompatSection, CompatWarning, CompatNodeCard, and VersionDeltaList — making outdated copies, version differences, and rebuild banners all visual. It also supports "version pinning" to **lock a template to specific component versions**, preventing templates from silently breaking after node upgrades.

## 5. Things Not in the Highlight — But Equally Important

### 5.1 Network Performance: P99 Latency Down 41%

The GetTapFile critical sandbox startup path was rewritten with a three-tier strategy: cache hit takes the fast path (0 syscalls), pooled fd already closed takes the warm path (2 syscalls, skipping the expensive restoreTap), and stateless takes the full restore path. The fdserver's JSON response carries ifindex, letting cubelet skip `netlink.LinkByName` — a frequently overlooked but expensive concurrent serialization point. The TOCTOU race between EnsureNetwork/ReleaseNetwork was also replaced with per-sandbox creating guard channels instead of singleflight.

BMI5 environment benchmarks:

- Network P50: 35.3 → 23.1ms (**-35%**)
- Network P99: 86.6 → 51.2ms (**-41%**)
- Overall startup P50: 106.1 → 92.0ms (**-13%**)
- Throughput: 194.8 → 209.8 sandboxes/s

### 5.2 Template Build: Peak Disk from 4.2x to 1.2x

The template image build pipeline underwent a complete rewrite — introducing skopeo + umoci for a **daemonless path**.

Cube made disk optimizations in 5 areas: including piping Docker export stdout **through a 1 MiB pipe directly to `tar -xf stdin`**, eliminating the intermediate rootfs.tar; ext4 space estimation using a **triple-overhead precision model** (256 MiB fixed + data 10% + 1 KiB per file), aligned at 256 MiB boundaries, and more.

The final result: **peak disk usage dropped from ~4.2x image size to ~1.2x**. File-level SHA256 fingerprinting also enables cross-build deduplication. The SDK's `POST /templates` now supports DNS, egress CIDR, image registry authentication, command/args, network type, and node scope options.

Additionally, **at the scheduling level, configurable oversubscription and custom node affinity are now supported. A series of core bugs were also fixed**, such as: disabled internet no longer automatically allows DNS (previously a hidden vulnerability under deny-all policy, affecting security semantic consistency), and DNS server IPs are automatically added to AllowOut (previously required manual configuration — a must-hit for newcomers).

## Coming soon...

v0.4.0 fills in the "egress governance" and "observability" pieces, but we'll continue pushing these capabilities forward in the next release:

- **ARM physical machine support**: Cube is currently an x86-first runtime, but the reality of AI infrastructure is already dual-architecture — more teams want to "run Agents on ARM." The next Cube release plans to bring the MicroVM kernel, guest-image, and cube-agent to full ARM support.

- **Volume SDK**: v0.3.0's snapshot/clone/rollback solved "sandbox state" management, but user data and sandbox lifecycle remain coupled. The upcoming Volume SDK will allow independent creation, cross-sandbox mounting and migration, and independent snapshot/backup. This means Agents can have a cross-session, cross-instance "workspace" rather than just a one-time execution environment.

- **Auto-pause and resume (autopause)**: Idle sandboxes holding memory and CPU quotas is a real cost — during the gaps when Agents wait for user input, long-task callbacks, or the next RL rollout round, sandboxes are mostly "running empty." Autopause will let sandboxes fully release host memory and CPU quotas when idle, keeping only disk state, and wake up in milliseconds on access. Combined with v0.3.0's snapshot mechanism, Cube can achieve "pause" at near-zero memory footprint and "resume" at sub-100ms.

Cube has moved past the "build a high-performance MicroVM sandbox" stage. Starting from v0.4.0, the question it answers is: **when AI Agents truly run at scale in production, what governance capabilities should the sandbox provide?**

If you're building Agent workflows, Agentic RL training platforms, or developer-facing code execution services, come check out the project on GitHub — file issues, send PRs, or just say hi.

**GitHub repo:** https://github.com/TencentCloud/CubeSandbox

**v0.4.0 full Release Notes:** https://github.com/TencentCloud/CubeSandbox/blob/master/docs/changelog/v0.4.0.md
