# Recovering sandbox lifecycle operations

A missing worker route does not prove that a sandbox has stopped. The create-time
host stored in the canonical specification may be stale after a cross-node
resume, so it is not used as fallback authorization for a destroy request.

When a sandbox still has a durable specification but no current route, destroy
returns an availability error and preserves the specification and references.
Restore the owning worker's registration or reconcile its current routing record,
then retry deletion with the full sandbox ID. A missing or unavailable explicit
HostID also reports worker unavailability rather than claiming the sandbox is
absent.

A successful asynchronous delete response acknowledges enqueueing, not physical
VM destruction or reference cleanup. Use synchronous deletion when the caller
needs that result. Failed reference release retains the canonical specification
so a later retry can finish cleanup; monitor worker/task errors until that retry
succeeds.

There is no automatic age-out or force-delete path for permanently lost workers
in this change. Before administrative recovery, fence the old worker and verify
that its VM cannot resume, identify any migrated or paused owner, and reconcile
snapshot and volume references. Do not delete the specification or release
storage references solely because a heartbeat or route is missing. A dedicated
force-delete API needs an explicit fencing contract and is separate from normal
retry behavior.
