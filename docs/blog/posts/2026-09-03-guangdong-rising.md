---
title: "Who Has the Right to Delete a Sandbox: Building a Multi-Tenant Sandbox Platform on CubeSandbox"
date: 2026-09-03
author: Feng Jiaqi (Senior Algorithm Engineer, Guangdong Rising)
description: "Creating a sandbox takes a single API call. Deleting one requires answering a string of questions: does it still have an active session? Is this cleanup request a latecomer? Who collects sandboxes left behind by crashed replicas? The Guangdong Rising team consolidated the sandbox capabilities of two product lines into a standalone service — the sandbox execution layer of the SIN PaaS (AI platform) — and answered these questions with leases, fencing tokens, tri-color mark-and-sweep collection, and admission control."
featured: true
weight: 3
---

# Who Has the Right to Delete a Sandbox: Building a Multi-Tenant Sandbox Platform on CubeSandbox

By｜Senior Algorithm Engineer, Guangdong Rising · Feng Jiaqi

**Editor's note:** Creating a sandbox takes a single API call. Deleting one requires answering a string of questions: does it still have an active session? Is this cleanup request a latecomer? Who collects the sandboxes left behind by crashed replicas?

Guangdong Rising runs AI-Agent-generated code on two product lines (SIN Builder and Dasheng AI). The team consolidated sandbox capabilities into a standalone service — the sandbox execution layer of the Guangdong Rising SIN PaaS (AI platform) — and answered the questions above with leases, fencing tokens, tri-color mark-and-sweep collection, and admission control. This article records the full journey of this governance system: from two separate implementations, through one leakage incident, to final convergence.

## The Starting Point: Two Implementations and One Incident

Before choosing Cube Sandbox, we had four problems to solve: 1) each of the two product lines had written its own sandbox capability — they were not interchangeable, with inconsistent lifecycles, leases, and reclamation policies, making issues hard to locate and hard to govern; 2) Agent sessions are long-lived and stateful, and the same session must reuse the same sandbox, which requires leases and fencing tokens designed at owner granularity — otherwise a late cleanup request would directly delete the new generation's lease and kill a running session; 3) under multi-replica deployment, quota and reclamation are hard to satisfy at both ends: you can neither garbage-collect a running sandbox nor let sandboxes left behind by crashed replicas leak forever; 4) sandbox cold-start latency translates directly into user wait time, and masking it with a self-built warm pool carries high maintenance and consistency costs.

The first problem persisted for a long time. Dasheng AI (an enterprise-grade AI assistant, internal codename dasheng-next) had embedded sandbox capability into its product code since May 11, 2026; sin-builder-core, the backend of SIN Builder (an AI application generation platform), only integrated Cube Sandbox (hereinafter "Cube") independently in early-to-mid June, with our own runtime integration and event parsing logic. The two lines evolved separately and never converged. What finally forced the consolidation was an incident on June 18, 2026.

That incident took the form of a death spiral. The internal investigation report identified the root cause as a blocked write path in cube-api / CubeMaster: when the incident occurred, 704 sandboxes had accumulated on the platform, all in running state with endAt equal to startedAt (never renewed since creation); on the host, 732 containerd-shim-cube-rs runtime processes occupied 304 GB of memory in total — 385 of them from the idle warm pool, and another 319 with no session ownership, belonging to 5 historical instances that no longer existed. The read path `GET /sandboxes` worked fine, returning in seconds; but the write path — POST and DELETE — all timed out with 408. The reclaimer identified 216 sandboxes pending cleanup every round, yet the actual deletion count was 0 — kill failed 1,359 times, create requests failed 10,214 times, all 408. **This formed a closed loop: sandboxes left by historical instances kept accumulating, the sheer volume crushed write operations, create and kill both timed out, old sandboxes could neither be deleted nor replaced with new ones, the platform was dragged down further, and both product lines became unavailable at the same time.**

This incident had three direct impacts on subsequent design. First, the 385 leaked sandboxes came from the idle warm pool, proving the warm pool was the biggest leakage source — and a month later, we measured Cube's official hot-start at sub-second level, so the warm pool was retired entirely. Second, the 5 historical owner instances and 319 unowned sandboxes showed we lacked a single accounting authority — the first problem the new standalone service had to solve. Third, during post-incident reconstruction we confirmed two hardening items: write operations need circuit breaking, and GC kill must not be serially awaited.

## Convergence: The Only Sandbox Boundary

One week after the incident, we built common-sandbox-runner, a standalone FastAPI service on top of Cube Sandbox, to consolidate sandbox capabilities. It is the sandbox execution layer component of SIN PaaS.

- SIN Builder and Dasheng AI are deployed in the same K8s cluster as the runner, under different namespaces. They call the runner through a single interface, `POST /v1/sandbox-runs` (which streams started, progress, and completed events over SSE). They hold no sandbox state themselves and never access Cube directly.
- The runner is the only side of the entire system that writes to Cube. It is currently deployed as a single replica, and the pod holds no authoritative state — leases, admission counts, GC candidate lists, and all other cross-request state live in Redis. Multi-replica safety is already supported by design.
- The Cube platform and the runner are deployed on the same node. See the dashed lines in the diagram below for platform-side behavior: the sweeper triggers pause based on endAt, without checking whether any command is executing inside the sandbox.

![Deployment and call relationships](./assets/2026-09-03-guangdong-rising/01-deployment-topology.jpg)

*Figure 1: Deployment and call relationships*

Every request carries a namespace (product/team boundary) plus an env (dev/staging/prod), from which the runner derives a unified `owner_id` and writes it into the sandbox's metadata. Whether for cleanup, cancellation, or troubleshooting, everything is looked up with the same predictable key format, and every sandbox can be traced back to a concrete owner. The specific rule: unless the caller explicitly specifies otherwise, `owner_id` is concatenated in a fixed order from namespace, env, project ID, and session run ID — human-readable. For business sandboxes, the runner writes namespace, env, `owner_id`, and `managed_by` together into the sandbox metadata. The payoff is most obvious during troubleshooting: pick up any business sandbox and, without querying any internal state, you can read directly from its metadata which product line, which environment, and which session it belongs to. Templates use a dual-template design — one main template plus one lightweight template; callers can override with `template_id` to pick an image based on the task's resource footprint. On the Cube side, we call the official SDK's create and connect directly, without wrapping another self-built abstraction; lifecycle configuration uses `on_timeout=pause` plus `auto_resume`, so idle leases enter a "soft-reserved" state instead of being killed outright, and the session restores its state when it comes back. At three moments — create, reconnect, and the heartbeat of an active run — we call `set_timeout()` to renew and validate the returned endAt; if renewal fails, we proactively terminate the run rather than let it keep running with an uncertain lifetime.

From acceptance to completion, a run follows a fixed sequence:

- **Before started**, an admission slot is acquired atomically via a Redis Lua script — bucket-level quota and global cap are checked together, with a maximum wait of 30 seconds; on timeout it returns a retryable CAPACITY_EXCEEDED. The key here is merging "check the quota" and "hold the quota" into one atomic operation: a single Redis Lua script executes "read count, compare limit, increment if passed" on the server in one shot — the bucket limit is always checked, and the global cap is checked when the global gate is enabled — leaving no window for any other request to slip in between, so even under multi-replica concurrency, two requests can never both see "one slot left" and oversell. Releasing a slot also goes through an atomic script: if creation fails, it rolls back on the spot; for a successfully created sandbox, the slot is released together with the owner record only after confirming the sandbox truly no longer exists, and any count accidentally left behind in between is swept up by the GC as a fallback. After getting a slot, if an existing owner lease is hit, we connect to reuse the same sandbox; otherwise we create a new one. After obtaining the sandbox, we also run a data-plane health probe (3 outer retries × 3 inner retries): between create returning and the data plane actually becoming usable there is an activation delay of several to a dozen seconds, and sandboxes that fail the probe are discarded and rebuilt — we never keep running with a sandbox whose usability is uncertain.
- **From started to progress**, the workspace and context are written in, and the entry process is launched in detached mode via nohup, so short commands return immediately.
- **From progress to completed**, a single-loop, strictly serial poll runs: scan artifact increments, send progress events, read `entry.exit` to determine completion, check cancellation flags and the deadline, heartbeat every 30 seconds while rebasing endAt along the way. No matter which exit the loop takes, a finally block always executes an idempotent pkill, ensuring no residual processes remain.

![SSE sequence of a run](./assets/2026-09-03-guangdong-rising/02-run-sse-sequence.jpg)

*Figure 2: SSE sequence of a run*

This "detached launch plus strict serialization" design was earned from a pitfall we hit on Cube. The early implementation was "blocking long commands plus concurrent polling" — both acting on the same sandbox at once would make envd block each other, once causing `app_generation` to hit ReadTimeout or hang. We first disabled the file stream by default, and only re-enabled it after switching to detached launch plus strict serialization.

This flow solves "how a run finishes," but a more fundamental question remains: **how does the same session guarantee it always finds its own sandbox — and only its own? That relies on the lease mechanism.** These two interfaces are our own design, with deliberately narrow semantics. On the acquire side: `POST /v1/leases` carries `owner_id` (along with namespace and env); the caller may also attach a self-generated `lease_id` for each generation of the session, serving as an epoch marker for conditional release. If this owner already has a live lease and the template is compatible, the interface returns the same `sandbox_id` marked reused=true — when a session resumes, it gets back the exact same sandbox. On the release side: `POST /v1/lease-releases` must carry both `owner_id` and `lease_id`; only when this pair exactly matches the generation (epoch) currently held by the server will the sandbox actually be killed and the slot released (cleanup completes asynchronously). On mismatch, only that expired generation is sealed off as void — it never falls back to "delete the current lease." This rule solves exactly the problem mentioned earlier — a late cleanup request may hit a situation where the old lease has expired and a new generation has taken over the same session; deleting without validation would kill the sandbox the new generation is actively using. After introducing epoch matching, this problem disappeared completely.

## Safe Reclamation: Tri-Color Marking and the Division of Labor with the Official Lifecycle

Our rewritten reclaimer borrows the idea of tri-color mark-and-sweep with generational grace periods from the JVM and Go.

- Black is the root — runs, leases, and operation locks with fresh heartbeats are never reclaimed;
- Gray marks sandboxes within their grace period — deletion forbidden;
- White — only sandboxes with no root reference for two consecutive confirmation cycles and an unchanged generation number are actually swept.

![Tri-color mark-and-sweep illustration](./assets/2026-09-03-guangdong-rising/03-tri-color-marking.jpg)

*Figure 3: Tri-color mark-and-sweep illustration*

Before sweeping, a distributed lock is acquired for each sandbox, then generation and the root set are double-checked; only after the kill succeeds or the query returns 404 is the owner metadata deleted and the corresponding admission quota released.

Of the four parameters, only the 30-second heartbeat is a self-defined cadence; the other three are derived from it: black-root determination at 120 seconds (4× heartbeat, tolerating 3 consecutive misses), newborn grace period at 180 seconds (3× the GC cycle, covering data-plane activation delay), and confirmation over 2 consecutive cycles (a single listing cannot be trusted — the platform may omit paused sandboxes).

![The four reclaimer parameters and their rationale](./assets/2026-09-03-guangdong-rising/04-gc-parameters.jpg)

*Table 1: The four reclaimer parameters and their rationale*

**This reclaimer was honed through three rounds of incidents sharing the same root cause — it was not designed correctly in one shot:**

The first round was a production incident on July 13, 2026: a sweeper failure left 100 zombie sandboxes (93 of them paused, with endAt overdue by 23 to 41 hours) occupying the full quota, making every sandbox-dependent feature unavailable. This gave birth to the endAt overdue fallback — beyond a 1,800-second grace period, do a best-effort kill and release the accounting.

**The second round had the same root cause as the first but recurred in a different form.** Our GC had a rule: "as long as a sandbox still holds a lease, treat it as a root object and never reclaim it." The rule was meant to protect sandboxes in use, but it failed to consider that the lease itself might have already expired. Compounded by the Cube platform returning paused sandboxes in the sandbox listing (so our force-kill logic never reached them), the end result was that paused sandboxes with long-expired leases were perpetually judged "root objects, undeletable," with 85 and 173 expired zombies piling up on the two product lines respectively. Our fix was to split the "should it be reclaimed" decision from the "mechanism" layer into a "policy" layer: each resource bucket explicitly declares its own retention period (in seconds), and only when all three conditions hold simultaneously — the bucket's retention period is greater than 0, the sandbox is indeed in paused state, and it has exceeded the retention period — is it judged reclaimable; in all other cases the original "never reclaim" behavior is preserved.

**The third round was a subtler self-locking problem.** Our GC relies on "the most recent successfully fetched listing" as the reconciliation baseline. If the fetch fails or the result looks abnormal (say the listing suddenly goes from non-empty to empty), the GC skips that round, does nothing, and retries the next round. But one edge case turns this protection into a trap: when the last sandbox in the system triggers auto-pause normally, the listing drops from "one" directly to "zero." This data shape looks identical to "the Cube platform is completely unreachable and no listing can be fetched" — the two situations are indistinguishable to us. The original logic kept skipping reconciliation whenever it saw this "suspected disconnect," and once quota ran out, no new sandbox could come in to unlock the state. The fix is to cap the number of "skips" (3 consecutive rounds by default); beyond that, reconciliation is forcibly resumed and a log is recorded. The "blind" window in between is covered by querying individual sandbox statuses as a fallback.

The same round of changes also added a reverse test: let an idle lease go through two confirmation cycles, then assert "the number of renewals during this period must be 0," preventing the GC from casually renewing idle sandboxes during scans. Once renewed, that sandbox would never be handled by the official autoPause.

The impact of resume on admission counting: resume-then-kill currently lives only on a branch and hasn't been merged into main. The background: Cube v0.5.1 refuses DELETE on paused sandboxes (error code 130593); waking via connect() works under normal circumstances but is best-effort — a single transient failure makes kill keep returning 500, and that sandbox may survive every GC round. One key question can be answered directly here: resume does not cause admission counts to overflow, because counting is per owner record — a paused sandbox never releases its slot, and killing it after wake-up is not double-counted; a slot is released only after kill succeeds or 404 is confirmed. But physical resources are briefly occupied — 2C/2G per sandbox; waking and killing all currently backlogged paused sandboxes one by one would transiently occupy 212 vCPU / 217 GB, which is exactly why we hope the platform will directly support DELETE on paused sandboxes.

![Overall evolution roadmap](./assets/2026-09-03-guangdong-rising/05-evolution-roadmap.jpg)

*Table 2: Overall evolution roadmap*

## Three Outcomes After Adopting Cube Sandbox

The reclamation mechanism solves how to safely release a sandbox after it's used, but before a sandbox is even created, a gate is needed to control how many can be opened simultaneously. We implement admission control with Redis-backed admission counting: bucket-level quotas by namespace and env plus a global gate, with separate default capacities for test and production environments, a maximum wait of 30 seconds per request, and multiple replicas acquiring and releasing against the same counters.

Since this mechanism went live, there have been three fairly direct outcomes:

**First, sandbox startup latency dropped significantly.** Measured hot-start from baked templates: median 180 ms, P95 at 210 ms, and cold start consistently within one second. Precisely because official startup is fast enough, we retired our self-built warm pool entirely.

**Second, architectural complexity dropped significantly.** Retiring the warm pool plus switching sandbox scripts to pre-baking removed about 1,350 lines of code net in this round (193 added, 1,542 deleted), with the sandbox runtime's core file alone shrinking by 650 lines — WarmPool, PoolRegistry, pool branch logic, the `skip_pool` switch, the entire stateful chain was removed. The sandbox logic previously written separately by the two product lines converged into 1 shared service, guarded by 198 test cases.

**Third, the admission logic limit was relaxed from 200 to 500 sandboxes, breaking the quota deadlock caused by zombie leases** — without adding any extra machine resources.

A clarification on the number "500": first, 500 is the logical limit of the runner's own admission gate, not the real capacity limit of a single Cubelet node, which we have not tested. Second, we have not done stress testing — 500 was not derived from stress tests, nor have we done layered stress testing across CPU, memory, network, and scheduling, so we cannot give real bottleneck data. The only known figure is 2C/2G per sandbox; the true limit depends on the physical capacity of the Cube cluster, which we have never approached — all capacity-type errors on the Cube side in production are CapacityExceeded thrown by the runner's admission gate, rooted in zombie leases occupying the accounting, not Cube reporting insufficient resources; the 106 zombie sandboxes mentioned earlier, 212 vCPU / 217 GB, stayed on the cluster for up to 32 days without any platform-side alert. The current deployment is single-replica, daily scale is within a thousand sandboxes, and the 500 limit has never been truly approached.

## Conclusion

Looking back: our two product lines each implemented their own sandbox logic for five and a half weeks; one leakage incident pushed us to extract the service within two days, complete the overall switchover in another two and a half weeks, and finally spend four days intensively refactoring lifecycle management.

The current production reality (data as of 2026-08-13): 106 sandboxes, all paused, none running, the longest resident for 32 days; the GC runs normally, but only 4 candidates actually meet the reclamation criteria — not because the GC is broken, but because the default per-bucket retention period is 0 (never reclaim, to avoid affecting legacy usage), and the buckets these sandboxes live in happen to have never declared a retention period, so they accumulated up to the quota limit. The price of this conservative "never reclaim by default" choice was a month of accumulated zombie sandboxes.

The truly hard part of a sandbox platform has never been how to create a sandbox, but how to safely delete one. Every "delete or not" judgment must find a defensible boundary between two risks: killing a session that's still in use, and letting leaks run free.

*Thanks to Feng Jiaqi of Guangdong Rising and his team for their contribution to this article.*
