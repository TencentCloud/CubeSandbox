# Node resource admission

Cubelet checks available resources and saves the sandbox's `CREATED` record under the same node-local lock, before starting its VM. Concurrent creates, snapshot clones, and restores cannot reserve the same capacity using a stale scheduling heartbeat. VM startup and probing run outside the admission lock.

A restart replaces its retained allocation once, while every other sandbox remains accounted. OCI/runc containers keep their existing save path because they do not carry MicroVM overhead metadata.

## Quota and accounting

Admission and heartbeat reporting use the same quota policy. Explicit `host.quota.mcpu_limit` and `mem_limit` values take precedence. Automatic values preserve the existing overcommit policy: twice the logical CPU count, and five quarters of physical memory in MiB, rounded up. New instances are rejected when the quota cannot be determined or an existing live instance lacks resource metadata.

Requests include runtime overhead through `ResourceWithOverHead`. CPU is accounted in millicores and memory is checked in bytes. Paused instances follow `paused_resource_release_ratio`; `mvm_limit` also limits the instance count. A record published in memory before a failed save continues to count until ordinary failure cleanup removes it. There is no separate temporary reservation ledger.

## Capacity rejection and retry

Cubelet reports `HostResourcesNotEnough` (130591) when local admission fails. CubeMaster may retry another node within its existing retry budget when the request is not pinned to a host. A pinned restore or an exhausted retry budget returns the existing `SelectNodesNoRes` (130597) capacity error.

Scheduler filters accept requests that exactly match remaining CPU or memory quota. Memory comparisons retain byte precision instead of truncating demand to MiB. Host memory headroom and CPU utilization checks still apply independently.

## Upgrade and verification

Upgrade CubeMaster first, then upgrade one Cubelet as a canary. Confirm that existing instance processes and state directories are preserved. Submit concurrent clones whose total demand exceeds the node quota: admitted allocations must stay within the published quota, and excess requests must receive an explicit capacity error.

After deleting successful instances, verify that their capacity can be reused and that rejected requests leave no VM, storage, network, or resource records. Expand the rollout after these checks pass.

Regression cases cover CPU and memory boundaries, byte precision, paused accounting, instance-count limits, 10/100/1000 contenders for the last allocation, conservative accounting after a failed save, capacity reuse after deletion, and Master retry classification. These cases do not replace a live concurrency test.

Runtime classification uses the configured runtime type, since MicroVM and OCI
sandboxes both carry runtime metadata. Missing runtime metadata is treated
conservatively as MicroVM metadata. Resume uses the same automatic host quota
calculation as create. Operator retry exclusions also apply to capacity errors;
excluded errors return the capacity response without rescheduling.
