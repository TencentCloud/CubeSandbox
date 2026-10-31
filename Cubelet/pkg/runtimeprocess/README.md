# TAP descriptor ownership checks

Ownership is determined from each process's `fdinfo` `iff` field, independent of executable names. This includes renamed runtimes and helpers holding a passed descriptor. Cubelet's own retained descriptors are excluded.

Cubelet must be able to inspect every process that can receive a TAP descriptor. Missing processes and closed descriptors are tolerated; other procfs errors or an expired scan budget fail closed. A restricted procfs mount or a process in an inaccessible user namespace prevents reuse and keeps cleanup pending; creates return an inspection error instead of allocating TAPs that cannot later be cleaned. Fix process visibility before enabling this reuse policy on such a host.

An inspection error releases the temporary Ready reservation without consuming the pool, then returns an error without allocating a fresh TAP. A confirmed foreign owner quarantines the queue, and acquisition tries another Ready entry or a fresh TAP. Reacquiring the same Active owner's TAP does not reject that owner's legitimate handoff.

Scans cost O(total visible file descriptors). Cleanup checks before changing the datapath and again before publication; draining occurs only after the final check. Measure create latency and cleanup throughput at the intended process and pool counts. These checks have not been benchmarked on a large production node. Caching a negative result across handoffs would weaken ownership safety and is intentionally avoided.

A quarantined queue retains its IP until physical ownership ends. It continues
to count toward the configured inventory to avoid unbounded replacement growth;
on-demand creation can still use other free addresses. Use `cubecli diag tap-holders --iface <tap-name>` to identify the holder and
its start time. Read `cubecli diag reclaim --help` before reclaiming a confirmed
stale runtime; reclaim verifies the process identity before signalling it.
Repair process visibility first if the ownership scan is incomplete: the
diagnostic command tolerates inaccessible processes and cannot certify reuse. Do not
delete its pool record or release its IP while a foreign process holds the queue.
Cleanup returns an error while the durable deleting record remains pending, so
callers must retry after physical VM teardown instead of treating it as success.

The default destroy and cleanup flows in `Cubelet/config/config.toml` run
`cubebox` before `network`. `cubebox.Destroy` calls `waitSandboxRuntimeGone`
before returning success. Preserve this ordering in custom workflow settings.
A still-open foreign descriptor is therefore retained as a cleanup failure,
not reported as a completed release.
