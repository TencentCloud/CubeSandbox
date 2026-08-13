# Roadmap

---

## Coming Soon

### Cross-Node Pause and Resume

Suspend a running sandbox on one node and resume it on a different node, with full memory and filesystem state preserved. Unlocks flexible bin-packing, host drain workflows, and cross-node sandbox migration.

### Cross-Node Snapshot-Based Sandbox Launch

Create new sandboxes from a snapshot on a different node than the one where the snapshot was taken, backed by a cluster-shared, on-demand-loading storage layer. The scheduler picks the source node first and transparently falls back to any node with the snapshot synced, so callers get elastic, cluster-wide scheduling from a single snapshot without waiting for a full data copy.

### E2B API Compatibility

Close the remaining gaps between CubeSandbox's API surface and the E2B specification. The goal is full drop-in compatibility so that workloads and SDK clients targeting E2B can run against a self-hosted CubeSandbox cluster without modification.

### Control Plane / Data Plane Separation

Separate the control plane (cluster management, scheduling, health checks) from the data plane (sandbox create/run/snapshot) so that a failure or rolling upgrade of the control plane does not affect sandboxes already in flight. Achieving full end-to-end high availability requires that the two planes are independently deployable and fault-isolated.

### Sandbox Fault Recovery

Automatic detection and recovery of sandboxes in abnormal states — crashed VMs, stuck shim processes, and network partitions. Includes a configurable recovery policy (restart, rollback to last snapshot, or surface to caller) and improved observability around failure events.

### Scheduling and Operations Enhancements

Richer scheduling capabilities including resource-aware placement, affinity/anti-affinity rules, and priority classes. Also covers operational tooling: live resource rebalancing and node drain with sandbox migration.

---

## How to Influence the Roadmap

1. **Open an issue** with the `enhancement` label — feature requests are reviewed during sprint planning
2. **Vote with 👍** on existing issues to signal priority
3. **Start a discussion** — major design decisions happen in GitHub Issues before any code is written
4. **Contribute** — PRs are welcome; for anything non-trivial, discuss in an issue first

See [CONTRIBUTING.md](https://github.com/TencentCloud/CubeSandbox/blob/master/CONTRIBUTING.md) for contribution guidelines.
