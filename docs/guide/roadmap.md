# Roadmap

---

## Coming Soon

### Cross-Node Pause/Resume Performance

Cross-node pause/resume landed in v0.7.0 as a preview. The next step is performance: cut pause/resume latency and speed up snapshot transfer so that resuming on another node approaches same-node speed.

### E2B API Compatibility

Close the remaining gaps between CubeSandbox's API surface and the E2B specification. The goal is full drop-in compatibility so that workloads and SDK clients targeting E2B can run against a self-hosted CubeSandbox cluster without modification.

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
