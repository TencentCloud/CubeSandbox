---
title: Compute nodes report HeartbeatExpired while still online (clock skew)
author: Cold-E
date: 2026-10-22
tags:
  - heartbeat
  - clock
  - chrony
  - cube-ops
lang: en-US
---

# Compute nodes report HeartbeatExpired while still online (clock skew)

## Symptom

A compute node shows `Healthy=false` with `UnhealthyReason=HeartbeatExpired` in `cubeopscli node ls --json` (the WebUI `/nodes` page only marks the node with an amber dot — it renders no status text and does not surface the reason), yet the node is actually up — `cubelet` is running and posting heartbeats. Sandbox creation that should land on this node fails with `no ready replica` / `no more resource`, because the scheduler filters out `HeartbeatExpired` nodes.

The key tell: the node's `MetaDataUpdateAt` timestamp keeps advancing (the heartbeat IS arriving), but the node is still judged expired.

## Environment

- CubeSandbox v0.7.x, multi-node deployment (control node and compute node(s) on separate hosts)
- Compute node clock is not synchronized to a common time source (NTP/chrony)
- Related components: `cubelet` (heartbeat reporter), `cube-ops` (health evaluator)

## Root Cause

Cubelet reports a status heartbeat about once per second to `cube-ops` (`POST /internal/v1/node-agent/nodes/<node-id>/status`; `node_status_update_frequency` defaults to `1s` in `Cubelet/config/config.toml`; `<node-id>` is the node's identifier — under the K8s chart it is the node name, under the one-click/bare-metal path it is typically the configured node ID), and the `HeartbeatTime` field is stamped with the **compute node's local wall clock**. `cube-ops` evaluates health with its **own** local wall clock as `now`:

```go
// CubeOps/internal/nodemanagement/service/health.go
func EvaluateHealth(reportedReady bool, heartbeatTime, now time.Time, timeout time.Duration) HealthStatus {
    if heartbeatTime.IsZero() || now.Sub(heartbeatTime) > timeout {
        return HealthStatus{Healthy: false, UnhealthyReason: model.ReasonHeartbeatExpired}
    }
    ...
}

// MetadataTimeout returns the cubelet heartbeat expiry window (15s)
func MetadataTimeout() time.Duration { return 15 * time.Second }
```

The window is 15s. If the compute node's clock is more than 15s behind the control node's clock, the `HeartbeatTime` it stamps is more than 15s in the past from `cube-ops`'s perspective — so every fresh heartbeat is already "expired" on arrival. The heartbeat traffic is fine; only the two clocks disagree.

This is not a code bug — it's the standard distributed-systems assumption that all nodes share a common time reference. Compute nodes whose `chrony` has no server configured (`Stratum=0`, `Reference ID=00000000`) drift freely, and VM/PVM compute nodes are especially prone to drift.

## Diagnosis

1. Check the node state and reason. The default `node ls` table only prints `NODE_ID / NODE_IP / INSTANCE_TYPE / ZONE / CPU_TYPE / HEALTHY / SCHEDULING_DISABLED / HOST_STATUS` — it does **not** show `UnhealthyReason` or the heartbeat timestamp, so use `--json` (full fields, shows both) or `--score-only` (switches to a score/time view that includes `METADATA_UPDATE` but drops `HEALTHY`):

   ```bash
   cubeopscli node ls --json          # full fields: Healthy, UnhealthyReason, MetaDataUpdateAt
   # or, to watch the heartbeat timestamp advance (this view drops HEALTHY/UnhealthyReason):
   cubeopscli node ls --score-only    # NODE_ID / SCORE / METRIC_UPDATE / METRIC_LOCAL_UPDATE / METADATA_UPDATE
   # Look for: Healthy=false, UnhealthyReason=HeartbeatExpired, and a recent MetaDataUpdateAt
   # (--json is the mode that shows both the reason and the timestamp together)
   ```

2. Distinguish clock skew from a genuinely offline node. **If `MetaDataUpdateAt` stopped advancing days ago**, `cubelet` is actually stopped/dead — that is a real offline node, not this issue. **If `MetaDataUpdateAt` keeps updating** but the node is still `HeartbeatExpired`, the clocks disagree.

   Note the comparison is **relative**: `EvaluateHealth` checks `now.Sub(heartbeatTime)` using `cube-ops`'s wall clock (`now = time.Now()` on the control node), so it trips only when the heartbeat time is in the **past** relative to the control node's clock — i.e. when the compute node's clock is **behind** the control node's. Either end failing to sync can cause that: the compute node drifting slow, or the control node jumping ahead (its own chrony unsynced, or a VM pause/resume on the control node). The symptom itself is one-directional (a compute node that is fast stays healthy), but either clock can be the source of the relative skew, so check **both** — the compute node first, and if it looks fine, the **control** node too (`chronyc tracking` on each).

3. On the compute node, check whether chrony is synchronized:

   ```bash
   chronyc tracking
   # Stratum: 0  and  Reference ID: 00000000  => NOT synchronized, free-running
   # If the compute node looks fine here but the node is still HeartbeatExpired,
   # run the same check on the CONTROL node — it may have jumped ahead.
   ```

4. Measure the offset between the compute node and the control node. The simplest way is to compare wall clocks:

   ```bash
   # On the compute node, note the time, then SSH to the control node and compare:
   date +%s   # on compute node
   ssh <control-node-ip> date +%s   # on control node
   # The compute node must be BEHIND the control node: EvaluateHealth checks
   # `now.Sub(heartbeatTime) > timeout`, so only a slow compute clock reproduces
   # HeartbeatExpired. A compute node that is ahead stays healthy (the delta is
   # negative). A difference larger than 15s in that direction reproduces the
   # false HeartbeatExpired.
   ```

   Or, for a one-shot offset measurement against a specific NTP source, run this **on the compute node** (it prints `System clock ... wrong by <N> seconds` and does not modify the clock). The target must actually serve NTP — use the same `<your-ntp-server>` the control node syncs to (see Resolution step 1); if the control node is not an NTP server, point this at a real NTP source instead:

   ```bash
   chronyd -Q 'server <your-ntp-server> iburst'   # one-shot probe; prints the offset vs. that server
   ```

## Resolution

Both ends must be synchronized to a common time source — the comparison is relative, so fixing only the compute node does not recover the node if it was the control node's clock that jumped ahead. First verify both clocks are synced (diagnosis step 3 on each node), then synchronize whichever end is drifting.

1. Confirm the time source. If your control node runs chrony as a local NTP server (e.g. `local stratum 10` + `allow`), use it; otherwise use a public NTP pool or your organization's NTP server. Pick exactly the source the control node is synced to, so both clocks agree. If the control node itself is not synced (its `chronyc tracking` shows `Stratum=0`), fix the control node first — pointing the compute node at a source the control node does not use will not make the clocks agree.

2. Configure chrony on the compute node. The config path and service name differ by distribution:

   ```bash
   # ── RPM family (OpenCloudOS / TencentOS / RHEL / CentOS): /etc/chrony.conf, service chronyd ──
   sudo sed -i '/^server /d; /^pool /d' /etc/chrony.conf
   echo 'pool pool.ntp.org iburst' | sudo tee -a /etc/chrony.conf
   # Or, for an internal NTP server:
   # echo 'server <your-ntp-server> iburst' | sudo tee -a /etc/chrony.conf
   sudo systemctl restart chronyd

   # ── Debian / Ubuntu: /etc/chrony/chrony.conf, service chrony ──
   # (Ubuntu defaults to systemd-timesyncd; install chrony first if it is missing.)
   # sudo apt-get install -y chrony
   sudo sed -i '/^server /d; /^pool /d' /etc/chrony/chrony.conf
   echo 'pool pool.ntp.org iburst' | sudo tee -a /etc/chrony/chrony.conf
   # Or, for an internal NTP server:
   # echo 'server <your-ntp-server> iburst' | sudo tee -a /etc/chrony/chrony.conf
   sudo systemctl restart chrony
   ```

   > Use `pool` for a name that resolves to multiple addresses (like `pool.ntp.org`); `server` pins a single resolved address at startup. Use `server` for a single internal NTP host.

3. Restart chrony and verify the offset collapses to near zero (RPM family: `chronyd`; Debian/Ubuntu: `chrony`). After the restart, if the offset is large, chrony corrects it by **slewing** (gradually, default max slew rate ~8.3%/s) unless told to step — a skew beyond 15s can take much longer than one heartbeat cycle to close, and a skew of minutes/hours can take hours. Step the clock immediately if the node needs to recover fast:

   ```bash
   sudo systemctl restart chronyd    # RPM family — or: sudo systemctl restart chrony   (Debian/Ubuntu)
   chronyc tracking   # Stratum should now be non-zero
   sudo chronyc makestep   # step the clock now instead of slewing; safe to repeat if offset remains
   chronyc tracking   # offset should now be ~0
   # (or add `makestep 1.0 3` to the chrony config so the first 3 updates past 1s step automatically)
   ```

4. Once the clocks agree, the node turns healthy once the next heartbeat is evaluated — verify:

   ```bash
   cubeopscli node ls   # Healthy=True for the fixed node
   ```

No `cubelet`/`cube-ops` restart or config change is needed; once the clocks agree, the existing heartbeat stream is evaluated as fresh.

## References

- Heartbeat evaluation: `CubeOps/internal/nodemanagement/service/health.go` (`EvaluateHealth`, `MetadataTimeout`)
- Heartbeat reporting: `Cubelet/pkg/cubelet/node_status.go` (`HeartbeatTime: kl.clock.Now()`)
- Related: #455 (stale node health handling — this is a different failure mode: the heartbeat arrives on time, but the clocks disagree)
