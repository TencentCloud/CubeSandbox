# Direct-mode on-link neighbor resolution (scan-driven)

In **direct mode** (no cube-router, `EgressRedirectFlags == 0`), a sandbox may
egress to a destination that is on the node NIC's own L2 link (on-link). The
datapath needs the destination's MAC. This document describes how that is
resolved after the rework that followed PR #1321.

## Design rule

**The kernel neighbor table is the only source of truth for the MAC.** Neither
the datapath nor userspace fabricates ARP packets, mirrors the neighbor table,
or subscribes to netlink events. The split of responsibility is:

- **Datapath** (`mvmtap.bpf.c` `prepare_egress_l2`): only *reports* state — it
  runs `bpf_fib_lookup`, records the result, and *always forwards the packet*.
- **Userspace scanner** (`cubevs/direct_neigh_scanner.go`): owns all
  *scheduling* — when to trigger kernel neighbor learning, when to keepalive,
  when to garbage-collect.

## Datapath behavior (`prepare_egress_l2`)

For an on-link destination, in order:

1. **Positive cache hit** — `direct_neigh[daddr]` has a MAC and `now <
   valid_until_ns`: use the cached MAC, no fib lookup. `valid_until_ns` is
   *never* renewed on a hit (iron rule: a cache entry lives at most
   `CACHE_TTL` before being re-checked against the kernel).
2. **Negative cache hit** — `valid_until_ns` fresh but MAC is zero (a recent
   fib failure): skip the fib lookup, use the gateway MAC (fallback).
3. **Expired / never resolved** — run `bpf_fib_lookup(DIRECT|OUTPUT)`:
   - success (route out the node NIC): cache `fib.dmac` with
     `valid_until_ns = now + CACHE_TTL`, set `fib_ok = 1`, use it;
   - failure: invalidate the cache (MAC = 0, short `NEG_TTL`), set `fib_ok = 0`,
     use the gateway MAC (**fallback — the packet is never dropped**).

`last_used_ns` is refreshed at 1s granularity to bound map-write pressure on
high-pps flows.

**Off-link** destinations are untouched: the static gateway MAC pair, as before.

## Userspace scanner

A per-process goroutine (`StartDirectNeighScanner`) walks `direct_neigh` every
`SCAN_INTERVAL` and per entry:

- **GC**: `now - last_used_ns > GC_AFTER` → delete (traffic stopped; stop all
  triggering).
- **`fib_ok == 0`** (not yet learned): if `next_attempt_ns` is due, send a UDP
  trigger and schedule the next attempt with exponential backoff (1s, 2s, 4s,
  8s, 16s cap).
- **`fib_ok == 1`** (learned): reset backoff, and every `REFRESH_INTERVAL` send
  a keepalive trigger so the kernel re-validates the neighbor (drives NUD).

Trigger sends are rate-limited by a global token bucket
(`directNeighTriggerPerSec`) so a sandbox scanning a subnet cannot amplify into
node-level probing.

`send_udp_trigger` sends one UDP datagram from the node NIC's source IP to the
target. The learning event is the kernel's own ARP request/reply for the target
— independent of L4 — so no reply is awaited.

## Failure model (correctness depends on the scanner)

The scanner is in the correctness path only for *learning*; it never serves a
MAC itself.

| Scenario | Behavior |
|---|---|
| New on-link dest, hairpin network | Gateway fallback delivers immediately (zero loss); switch to direct MAC once learned. |
| New on-link dest, non-hairpin | Bounded blackhole (≤ scan interval + ARP RTT + NEG_TTL — a fresh negative cache can suppress the fib refresh for up to NEG_TTL after the kernel learns the neighbor), then converges. |
| Learned dest, MAC changes silently | Keepalive (≤16s) → kernel NUD re-validates → fib serves the new MAC; cache staleness ≤ CACHE_TTL. |
| Dest dead | fib keeps failing → negative cache (1s) + gateway fallback; scanner backs off to 1 probe/16s. |
| Scanner process restarts | No persisted state to restore; the map is rebuilt fresh; the datapath keeps serving via fib + fallback. |
| Scanner down long-term | Data plane re-freshes from fib at least every `CACHE_TTL`; new destinations in a non-hairpin network degrade to the pre-#1321 behaviour (never a frozen stale MAC). |
| Policy-routed NIC (fib ifindex mismatch) | Treated as unresolvable: gateway fallback, no trigger. |

## Parameters

| Parameter | Value | Notes |
|---|---|---|
| `SCAN_INTERVAL` | 200–500 ms | Main term in cold-destination learning latency. |
| learning backoff | immediate, 1s→16s cap | Dead target steady-state = 1 probe/16s. |
| `REFRESH_INTERVAL` | 16 s | Drives kernel NUD re-validation. |
| `CACHE_TTL` | 4 s | fib-result positive-cache lifetime; ≪ keepalive period so the cache is re-validated against the kernel at least once per cycle. |
| `NEG_TTL` | 1 s | Negative-cache lifetime after a fib failure. |
| `GC_AFTER` | 5 min | Idle entries are reclaimed. |
| trigger token bucket | 100/s | Anti-scan-amplification. |
| map capacity | 8192 LRU | Eviction → untracked (pure gateway fallback), still correct. |

## Migration from PR #1321

- The in-BPF ARP construction (`direct_egress_arp_request` + padding clear) and
  the `from_world` ARP learning (`learn_direct_neighbor`) are removed.
- `direct_neigh` changes from `{MAC, next_probe_at_ns}` to the trigger/cache
  struct above. Its content is disposable (re-learned via fib), so `Init`
  removes any stale pin before loading (this also drops a stale pin whose value
  size differs from the new struct).
- fib-first is retained and is now the only way a MAC is obtained.
- The gateway fallback is extended from off-link to on-link-unresolved, replacing
  the previous drop/probe behaviour. **First-packet loss and the 5-minute
  sacrifice packet are eliminated.**
