# TAP descriptor ownership checks

Ownership is determined from each process's `fdinfo` `iff` field, independent of executable names. This includes renamed runtimes and helpers holding a passed descriptor. Cubelet's own retained descriptors are excluded.

Cubelet must be able to inspect every process that can receive a TAP descriptor. Missing processes and closed descriptors are tolerated; other procfs errors or an expired scan budget fail closed. A restricted procfs mount or a process in an inaccessible user namespace can therefore prevent new sandbox creation and keep cleanup pending. Fix process visibility before enabling this reuse policy on such a host.

An inspection error releases the temporary Ready reservation without consuming the pool. A confirmed foreign owner quarantines the queue, and acquisition tries another Ready entry or a fresh TAP. Reacquiring the same Active owner's TAP does not reject that owner's legitimate handoff.

Scans cost O(total visible file descriptors). Cleanup checks before changing the datapath and again before publication; draining occurs only after the final check. Measure create latency and cleanup throughput at the intended process and pool counts. These checks have not been benchmarked on a large production node. Caching a negative result across handoffs would weaken ownership safety and is intentionally avoided.
