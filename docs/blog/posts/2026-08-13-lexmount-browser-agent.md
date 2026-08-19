---
title: "Putting the Browser Inside the Agent Sandbox: Lexmount's Hands-On Experience with CubeSandbox"
date: 2026-08-13
author: Xiong Xiuzhang (Lexmount Full-stack Engineer)
description: "Agent browser runtime is a 'composite workload.' It imposes four core hard requirements on AI Agent sandboxes: can outbound network reach the real internet, is the startup sequencing stable under batch creation, can per-sandbox runtime state be dynamically injected, and can real capacity be accurately perceived. The Lexmount team hit and resolved all four of these problems when integrating their browser runtime with CubeSandbox."
featured: true
weight: 2
---

# Putting the Browser Inside the Agent Sandbox: Lexmount's Hands-On Experience with CubeSandbox

**Editor's note:** Agent browser runtime is a "composite workload." It imposes four core hard requirements on AI Agent sandboxes: can outbound network reach the real internet, is the startup sequencing stable under batch creation, can per-sandbox runtime state be dynamically injected, and can real capacity be accurately perceived.

If any one of these fails, Agent behavior starts to drift. The Lexmount team hit and resolved all four of these problems when integrating their browser runtime with CubeSandbox. This article is their first-hand technical retrospective, and it also answers a more fundamental question: what do lightweight sandboxes sacrifice in pursuit of launch performance, and how should the business side compensate?

## 1. Background and Layering Strategy

### 1.1 Lexmount Business Background

Lexmount's self-developed Agent browser runtime (Lexmount Insight Flow) has a core use case: enabling AI agents to execute web tasks at scale and reliably. This business form inherently imposes several "must-haves" on the underlying sandbox — **real internet access** (a prerequisite for browser tasks), **batch concurrent launch** (multi-user parallelism / RL batch rollout), **per-sandbox runtime state injection** (multi-tenant API keys, task context), and **accurate capacity perception** (no inflated numbers).

During the selection phase, we searched the open-source community for tools that could "launch many lightweight sandboxes." After benchmarking OpenSandbox and Cube Sandbox, we found Cube had lower resource usage, so we chose the latter. Following from v0.1.0 to v0.5.1, around the four boundaries above, we hit four pitfalls: no public network access, template creation failures, environment variable loss, and phantom resource oversell.

### 1.2 Layering Strategy: Not Every Agent Needs to Go Entirely Inside the Sandbox

Before diving into specifics, let's share our overall sandboxization path strategy — it determines which chain each of the four problems appears on. We split sandboxization into two paths, with the business orchestration layer doing policy routing based on risk level, resource footprint, and business form.

**The first path only isolates Bash.** For low-risk, short-execution scenarios like general conversation, Q&A, and lightweight orchestration, the Agent Loop itself stays in the business orchestration layer; only `Bash/exec` tool calls enter the sandbox. Sandboxes are disposable and stateless. The advantage of this path is lightweight — short sandbox lifecycles, frequent start/stop, no resident resources.

**The second path puts the entire Agent Runtime inside the sandbox.** For high-risk, resource-heavy scenarios requiring resident or native interaction — like code development, browser automation, and long-running tasks — the Agent's resident process enters the sandbox entirely, with dedicated CPU/memory and a persistent workspace. The advantage is thorough isolation — the Agent has a complete runtime environment inside the sandbox, can start/stop anytime, and resource usage is comparable to resident microservices.

Both paths share the same Cube control plane, which creates or reuses lightweight/full Sandboxes on demand and orchestrates lifecycles. There are also plans for RL batch rollout — large batches of short-lived sandboxes, which is exactly the scenario Cube-type lightweight sandboxes are best suited for.

## 2. Outbound Network: Making the Sandbox Actually "Open Web Pages"

For browser runtimes, network is the first hard constraint. Every page the Agent opens, every API it calls, must go from the sandbox to the public internet. On day one with Cube v0.1.0, we hit this boundary.

Cube v0.1.0 was released just before the May Day holiday, and created sandboxes couldn't reach the public internet. We reproduced this across multiple environments, ruling out our own environment issues. Although the community released a fix right after the holiday, a new pitfall appeared after upgrading — creation reported success, but templates occasionally failed to build, and even when built, sometimes still couldn't connect.

We wrote many test scripts and cases, confirmed DNS and direct public IP connections both timed out, and first ruled out DNS resolution issues. We then captured packets and found that the SNAT session was established, but the connection was stuck in SYN-SENT, meaning the TCP three-way handshake didn't complete. Further checking of the host's default route and egress NIC were both correct. At this point, the IP layer and routing layer were fine, so we started suspecting below the IP layer.

We had AI help review the network-agent source code and found a suspicious point: the `getGatewayMacAddr()` function takes "the first reachable neighbor on the NIC" as the gateway. In a multi-neighbor environment, this MAC might not be the default gateway's MAC — if it picks the wrong one, the SYN packet goes out the NIC but is sent to the wrong next hop at L2, never reaching the gateway, and the connection naturally times out.

To verify this, we used `tcpdump` to capture packets on the egress NIC. The result was clear: the SYN was indeed sent from `enp3s0`, but the destination MAC belonged to a regular neighbor, not the default gateway.

Our fix was to change the `getGatewayMacAddr()` logic: first resolve the gateway IP from the interface's default route, then look up the neighbor entry for that IP, ensuring we get the real gateway MAC, not whichever reachable neighbor happens to be first. We also added two test cases: a multi-neighbor scenario where a non-gateway MAC ranks first, and a missing-default-route scenario. This PR ([#224](https://github.com/TencentCloud/CubeSandbox/pull/224)) was merged. During the investigation, we also found a separate MAC address overwrite issue and submitted a fix for that too.

**After this boundary was cleared**, the second path (entire Runtime in sandbox) truly had the prerequisite for running browser tasks. But the next boundary came quickly — not whether one sandbox can go online, but **whether many can be launched at once**.

## 3. Batch Launch: Before Launching a Hundred Sandboxes, Fix "Process Started ≠ Ready"

The second business requirement for Agent browser runtimes is concurrency. One user may open several page tasks simultaneously, multiple users use the Agent at the same time, and future RL rollouts will batch-launch short-lived sandboxes. All these scenarios share a common characteristic: **a single machine will receive a dense burst of sandbox creation requests in a short time**.

Dense creation naturally leads to a problem: startup sequencing races. The second pitfall we hit was here. The error message was `unknown service cubelet.services.images.v1.Images`, which looked like a Cubelet issue.

Initially, we did suspect Cubelet — version mismatch? Images service unregistered? Cubelet initialization exception? But checking showed the Cubelet process and gRPC ports were up; the problem only occurred after restarts or full one-click startup, and recovered on its own after waiting or adjusting startup order. So we started suspecting dependency initialization races.

Comparing the startup script, we found the root cause. The old logic started network-agent and immediately launched Cubelet, only checking network-agent with `/healthz` after Cubelet started. But Cubelet's network plugin initialization only waited 30 seconds — when network-agent was slow to start, Cubelet initialization would be incomplete, making the Images service unavailable.

So "network-agent not ready" didn't mean the process wasn't running, but that the process existed while `/readyz` hadn't passed yet, and Cubelet was pulled up too early. This also explained why it looked like a Cubelet error but the root cause was an upstream dependency not being ready. `/healthz` checks process existence; `/readyz` checks that dependency service registration is complete — the two are not interchangeable.

Our fix was to reverse the startup order: wait for network-agent's `/readyz` to pass before starting Cubelet and subsequent services. We also extended the script and Cubelet internal wait times to 120 seconds, and added a `NETWORK_AGENT_READY_TIMEOUT` environment variable for deployment tuning based on machine performance. This PR ([#304](https://github.com/TencentCloud/CubeSandbox/pull/304)) was merged.

**On the surface, this is a deployment script issue**, but in the context of browser runtimes, it's a "can it scale" problem — if dependencies are called before they're registered during batch launch, creations fail en masse. The business-side takeaway: for all components that depend on cross-process gRPC, readiness should be determined by `/readyz`, not `/healthz` or process existence.

## 4. Dynamic State: How to Inject Multi-Tenant API Keys into Sandboxes

The third requirement for browser runtimes is "per-sandbox runtime state injection on demand" — different users and tasks have different API keys, context labels, and downstream service addresses. We can't make a template for every combination. Ideally: the control plane generates a set of envs per request, passes them in at sandbox creation, and the Agent reads them after starting.

But we hit an architectural boundary in Cube. Running an Agent directly in the sandbox requires passing API keys and other environment variables. But passing a set of envs through the interface had no effect. Reading the source code, we found: `CreateSandboxRequest.containers` was hardcoded to `vec![]`, so the environment variables were dropped at this step.

We submitted PR ([#634](https://github.com/TencentCloud/CubeSandbox/pull/634)), attempting to fix it from CubeAPI's initialization chain, wiring `env_vars` into the container spec. But the maintainer responded that it was "incompatible with the original architecture design," and the PR was closed.

We later understood the technical reason for the rejection. Cube's sandbox creation goes through snapshot restore, not re-running the entrypoint. The template's runtime process was already started and memory-snapshotted during template creation; sandbox creation restores from this snapshot rather than re-executing the entrypoint. Therefore, changes to PID-1's `Process.Env` can't affect processes that already existed at snapshot time — those processes retain the environment from the snapshot moment and won't re-read per-sandbox injected variables.

The envd-backed command execution (`commands.run` / `run_code`) takes a different path: envd's `process.Start` reads and injects environment variables from an in-process defaults.EnvVars map, which is populated by envd's `/init` endpoint (handled by [#566](https://github.com/TencentCloud/CubeSandbox/pull/566)). The two paths are complementary but non-overlapping.

After confirming this wasn't a底层 defect but an architectural constraint of snapshot restore, we continued using the solution we'd already developed in early practice — a two-phase "control plane proxy, in-sandbox execution" design: the control plane exposes an Init API for auth, identity field protection, target instance resolution, and failure retry; the real `/v1/init` runs inside the CubeSandbox's Adapter or Agent business process, reaching the Agent through in-memory config updates, same-process env updates, or subprocess restarts. Idempotency is jointly guaranteed by the Sandbox control plane and the in-sandbox Adapter or Agent business process.

Looking back at this architectural constraint, we agree with the maintainer's judgment. The launch latency reduction from snapshot restore is real — hot start P95 is sub-second, which is precisely the prerequisite for browser runtimes to "batch concurrently." And env injection itself is a business-oriented scenario; explicit injection by the business side after restore is more controllable than relying on entrypoint re-run. **This is a typical "trade lower-layer flexibility for upper-layer performance" choice** — Cube chose to be aggressive on launch performance, clearly handing the responsibility for state injection back to the business side; once the business side knows the boundary, they compensate with a two-phase design, and the chain remains clean.

## 5. Real Capacity: From "Can Launch N" to "Dare to Use N"

Browser runtime is a resource-heavy workload — each sandbox contains a full Runtime plus a browser instance. The number business stakeholders care about most is: how many can actually run on one machine? Our earliest number was, in hindsight, inflated.

In the early version (v0.2.x), on a 16C/32G bare-metal server, we kept creating Cubes and got a relatively large number. After upgrading, we found that creating 30+ triggered resource limit errors, a big difference from our earlier understanding.

During troubleshooting, we initially suspected our own control plane and lifecycle management module had bugs. After ruling those out, we started combing through CubeSandbox's code and, with AI assistance, located the root cause: in v0.2.x's one-click deployment, CubeMaster used a script to write mock resource consumption metrics to Redis, which made CubeMaster's perception of resource consumption ineffective — it looked like there was plenty of headroom, but it was actually full. v0.3.0 fixed this issue.

This pitfall was essentially paying for historically insufficient testing rigor. The capacity numbers obtained by "endlessly creating empty sandboxes" were inflated — without real traffic, sandboxes don't truly consume resources, so naturally many can be launched. After the upgrade removed the mock metrics and resource locks took real effect, the inflated numbers were exposed.

We later summarized: for capacity assessment, the most direct signal is whether actual creation requests succeed. You need to cross-validate CubeMaster's real-time node health, remaining quota, and host-level `free`/`top` signals to form a systematic capacity monitoring and load-testing conclusion — any single signal can be "unreliable."

The insight from this is more than "fix a reporting bug" — it conversely defines our capacity planning principle: no single signal can be the basis for capacity conclusions; capacity must be measured under real business load. For browser runtimes, this means test cases must include real page loads and real Agent requests, not just launching a batch of empty sandboxes to see how many fit.

## 6. Current Status and Future Plans

We're currently in pre-production validation at v0.5.1, with plans to try v0.6.0's K8s / Volume capabilities. We'll be moving into customer privatization scenarios that strongly depend on K8s compatibility (the customer's infrastructure uses K8s), so K8s / Volume validation on v0.6.0 is our next priority.

Future plans also include RL batch rollout with bulk sandbox requirements — large numbers of short-lived sandboxes created, executed, and reclaimed concurrently. This is also where Cube-type lightweight sandboxes align with our business scenario.

## Lessons Learned

After working through all four directions, here are some takeaways worth documenting, which we hope will help peers evaluating or using Cube:

- **For outbound network issues, suspect L2 first.** When the SNAT session is established but the connection is stuck in `syn_sent` and the host routing is correct, the problem is most likely in L2 next-hop selection — `tcpdump` to directly check whether the SYN's destination MAC is the gateway MAC is the fastest way to confirm. This experience applies to all microVM + custom network plane sandboxes, not just Cube.
- **In batch scenarios, `/healthz` cannot replace `/readyz`.** At low single-machine concurrency, 30 seconds is enough; but once launch frequency increases, calling dependencies before they're registered will cause mass failures. For all cross-process dependencies, use `/readyz` for readiness determination.
- **Capacity numbers can't be measured by running empty.** The early resource reporting bug combined with "no real consumption when running empty" can significantly inflate capacity numbers. Capacity assessment must be done under real business load, with multi-source cross-validation.

Agent browser runtime is a composite workload that stress-tests the underlying sandbox more harshly than most scenarios. We followed from v0.1.0 to v0.5.1, reading source code root causes for every boundary, and submitted our own fixes. We hope this record is useful to other teams building Agent Runtimes.
