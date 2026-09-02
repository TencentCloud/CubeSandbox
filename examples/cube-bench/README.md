# cube-bench

A CLI benchmark tool for [CubeSandbox](../../README.md) that measures sandbox
creation and deletion latency at configurable concurrency levels.

Written in Go, it drives the CubeAPI HTTP endpoints directly using goroutines
for accurate, low-overhead measurements. Results are displayed in a rich
terminal UI (powered by [Charm](https://charm.sh) — bubbletea + lipgloss) and
can optionally be exported as JSON.

## Prerequisites

- Go 1.21 or later (`go version`)
- A running CubeSandbox deployment with CubeAPI accessible, **or** use
  `--dry-run` to simulate without a server
- A valid template ID (`CUBE_TEMPLATE_ID`) when targeting a real server

## Build

```bash
cd examples/cube-bench
make          # builds ./bin/cube-bench binary
```

Or manually:

```bash
go build -o cube-bench .
```

## Usage

```bash
./bin/cube-bench [flags]
```

### Environment variables

| Variable | Description |
|---|---|
| `E2B_API_URL` | CubeAPI base URL, e.g. `http://localhost:3000` |
| `E2B_API_KEY` | API key (any non-empty string for local deploys) |
| `CUBE_TEMPLATE_ID` | Template ID used for sandbox creation |

All env vars can be overridden by the corresponding flag.

### Flags

| Flag | Default | Description |
|---|---|---|
| `-c`, `--concurrency` | `5` | Max parallel in-flight requests |
| `-n`, `--total` | `20` | Total iterations |
| `-t`, `--template` | *(env)* | Template ID |
| `-w`, `--warmup` | `0` | Warmup rounds before measurement |
| `-m`, `--mode` | `create-delete` | `create-delete` or `create-only` |
| `-o`, `--output` | *(none)* | Export JSON report to file |
| `--host-mount` | *(none)* | Host mount list as a JSON array |
| `--network-policy`, `-np` | `none` | Network policy on create: `none` (no rules) or `rules` (create with egress rules) |
| `--api-url` | *(env)* | CubeAPI base URL |
| `--api-key` | *(env)* | API key |
| `--theme` | `auto` | Color theme: `dark`, `light`, or `auto` |
| `--dry-run` | `false` | Simulate API calls (no server needed) |
| `--dry-latency` | `80,30` | Dry-run latency: `mean,stddev` in ms |
| `--dry-error-rate` | `0.02` | Simulated error rate (0.0–1.0) |
| `--no-tui` | `false` | Disable interactive TUI |
| `--seed` | `42` | Random seed for the pre-generated request sequence |
| `--workload` | *(none)* | Workload preset: `burst`, `template_storm`, `mixed_spec` (empty = legacy mode) |
| `--rate` | `0` | Poisson arrival rate in requests/sec (`<=0` = as fast as possible) |
| `--lifetime` | *(none)* | Per-sandbox lifetime in seconds: `min,max` (uniform); client DELETEs when lifetime expires |
| `--templates` | *(none)* | Template pool: comma-separated `templateID[:weight[:cpuMillis:memMiB]]` (weight default 1) |
| `--dump-trace` | *(none)* | Write the pre-generated request sequence to a JSON trace file, then run normally (requires a scheduled workload) |

Without any scheduling flag (`--workload`/`--rate`/`--lifetime`/`--templates`)
the tool behaves exactly as before: all requests fire at once and each sandbox
is deleted immediately after creation.

### Examples

```bash
# Real server — 20 concurrent workers, 200 create+delete cycles
export E2B_API_URL=http://localhost:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>
./bin/cube-bench -c 20 -n 200

# Dry-run — no server required
./bin/cube-bench --dry-run -c 50 -n 500

# Create-only mode, export JSON report
./bin/cube-bench --dry-run -c 20 -n 200 -m create-only -o report.json

# Benchmark host-mount create requests
./bin/cube-bench -c 10 -n 50 --host-mount '[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]'

# Create with egress rules (CubeVS maps + CubeEgress policy push)
./bin/cube-bench -c 10 -n 50 -w 2 --network-policy rules

# Non-interactive output (CI / pipe)
./bin/cube-bench --dry-run --no-tui -c 10 -n 50

# Light terminal theme
./bin/cube-bench --dry-run --theme light -c 10 -n 100

# Scheduled workload presets (Poisson arrivals + per-sandbox lifetimes)
# NOTE: pass --concurrency explicitly — a worker slot is held for the full
# sandbox lifetime, so honoring the preset rate needs ≈ rate x mean lifetime
# slots (burst ≈ 3250, template_storm ≈ 1800); the default -c 5 stalls the
# dispatcher and the run degrades to ASAP pacing (the tool warns at startup).
./bin/cube-bench --workload burst -t <template-id> -c 3500
./bin/cube-bench --workload template_storm -t <template-id> --seed 7 -c 2000
./bin/cube-bench --workload mixed_spec -c 2000 \
  --templates 'tpl-1c2g:6:1000:2048,tpl-2c4g:3:2000:4096,tpl-8c16g:1:8000:16384'

# Ad-hoc schedule without a preset: 20 req/s Poisson, 5-30s lifetimes
./bin/cube-bench -t <template-id> --rate 20 --lifetime 5,30 -n 200 -c 400

# Dry-run a preset and keep the exact request sequence as a trace file
./bin/cube-bench --dry-run --workload burst --no-tui \
  -o report.json --dump-trace trace.json
# The same trace can then be replayed by an external scheduling simulator.
```

### Workload presets

A preset only supplies flag **defaults** — any flag you pass explicitly wins
(e.g. `--workload burst -n 20` keeps `total=20`).

| Preset | total | rate (req/s) | lifetime (s) | templates |
|---|---|---|---|---|
| `burst` | 500 | 50 | 10–120 | single (`-t`) |
| `template_storm` | 300 | 30 | 30–90 | single (`-t`) |
| `mixed_spec` | 400 | 10 | 30–300 | **requires `--templates`** with ≥2 entries, e.g. weights `6:3:1` for 1C2G/2C4G/8C16G |

In scheduled mode the whole request sequence is **pre-generated** from `--seed`
(same seed ⇒ identical sequence): Poisson inter-arrival times
(`Exp(λ)`, first request at t=0), uniform lifetimes in `[min,max]`, and
weighted random template picks. Each create body carries the picked
`templateID` and `timeout = trunc(lifetime) + 60s` as a server-side fallback
TTL; the client issues the DELETE itself when the lifetime expires. (The TTL
is set in `create-only` mode too — intentional, so lifetime-bearing presets
don't leak sandboxes when the client never DELETEs.) Two caveats on the
fallback: `timeout` is an *idle* timeout (see `docs/guide/lifecycle.md`), so
it only acts as a wall-clock cap because the bench never touches a sandbox
after create; and the seconds truncation makes the effective value up to 1s
shorter than `lifetime + 60s`. The report and JSON
export add queue-delay percentiles (scheduled vs actual start) and per-template
create counts/success rates.

**Throughput metrics in scheduled mode.** `total_time_s`/`throughput_qps`
measure the *whole run*, which ends only when the last sandbox is DELETEd —
i.e. they include the lifetime tail and are diluted by the workload's own
lifetime distribution. The JSON export therefore also reports
`dispatch_window_s` (wall-clock span of the dispatch loop, first to last
request release) and `dispatch_qps` (requests released per second of that
window), which reflect the arrival-side throughput the scheduler actually saw.
Both keys are emitted only for rate-paced runs (`--rate > 0`): without pacing
the "window" is just the client's release burst and the rate is meaningless.
For A/B comparisons of scheduler behavior, prefer `dispatch_qps`;
`throughput_qps` answers "how fast did the whole experiment finish".

Also note that `queue_delay_*` percentiles are **client-side dispatch delay**
(time the local dispatcher spent blocked on the concurrency semaphore, plus
sleep overshoot) — a self-check that the generator isn't throttling itself,
and ~0 when the concurrency guidance below is followed. They are *not*
scheduler-side queueing; scheduler queueing shows up in `create.*` latency
instead. Don't mix cube-bench's `queue_delay_*` with schedsim's scheduler-side
queue metrics in one `compare` run.

> **Concurrency for lifetime-bearing presets.** A worker slot is held for the
> full sandbox lifetime (create → lifetime sleep → delete). The lifetime sleep
> starts only after the create response returns (the client cannot know the
> sandbox ID earlier), so the effective residence per sandbox is
> `create + lifetime + delete` and the steady-state number of live sandboxes ≈
> `rate × (mean create + mean lifetime + mean delete)`. The `rate × mean
> lifetime` rule of thumb is fine for the ≥ 10 s preset lifetimes but
> underestimates for ad-hoc sub-second lifetimes where create/delete time is
> comparable. Set `--concurrency` at least that high (e.g. `burst`: 50 req/s ×
> ~65 s ≈ 3250) or the dispatcher stalls on the semaphore and the requested
> `--rate` is not honored; the tool prints a startup warning when it detects
> this.

### Trace file schema

`--dump-trace FILE` writes the pre-generated sequence before the run starts.
Requests are sorted by `arrival_ms`; `cpu_millis`/`mem_mib` come from the
`--templates` spec annotations (`0` when not annotated):

```json
{
  "workload": "burst",
  "seed": 42,
  "generated_at": "RFC3339",
  "params": {"rate_per_sec": 50, "lifetime_min_s": 10, "lifetime_max_s": 120, "total": 500},
  "templates": [{"template_id": "tpl-small", "weight": 1, "cpu_millis": 1000, "mem_mib": 2048}],
  "requests": [{"seq": 0, "arrival_ms": 0, "template_id": "tpl-small", "cpu_millis": 1000, "mem_mib": 2048, "lifetime_ms": 53210}]
}
```

`lifetime_ms` is the planned hold time measured from create completion (the
client sleeps it after the create response returns); true sandbox residence is
`create + lifetime + delete`, so trace replayers modeling occupancy should add
the create/delete tail themselves.

### Comparing runs (A/B report)

`compare` reads the JSON exports of two experiment groups (multiple seeds per
side) and renders a Markdown report: per-metric mean ± 95% CI (Student-t
quantiles — a normal z badly understates the interval at a handful of seeds),
absolute and relative change, and an improved/regressed verdict based on a
built-in direction table (latency/error/cv/fragmentation lower-is-better,
including cube-bench's own `create.*`/`delete.*` stat blocks and
bad-when-rising rates such as `restart_rate`/`preempt_rate`/`evict_rate`/
`retry_rate`; other rates/jain/throughput higher-is-better):

```bash
./bin/cube-bench compare --baseline default1.json,default2.json,default3.json \
                         --candidate new1.json,new2.json,new3.json \
                         --baseline-name default --candidate-name burst-spread \
                         -o compare.md
```

It accepts both cube-bench exports (`summary` plus the top-level
`create`/`delete` stat blocks are flattened recursively) and schedsim reports
(each entry of a top-level `rounds` array counts as one sample), so
real-cluster runs and simulator runs can be compared with the same command. A
metric is flagged in the conclusions when the verdict direction matches,
|Δ%| ≥ 5%, and — whenever both sides have n ≥ 2 — the delta exceeds the
combined CI half-widths, so small-sample noise is not flagged as a verdict.
When the baseline mean is exactly 0 (Δ% undefined), the test falls back to
the absolute delta — still CI-gated when possible — so a 0 → 0.5
`error_rate` catastrophe is flagged rather than silently dropped.
Directionless metrics join the "No direction" list only when the same
magnitude test (without the CI gate) marks the delta notable.

Caveats when reading verdicts:

- **Single-sample sides are not CI-gated.** With n=1 on either side there is
  no interval to compare against, so every |Δ%| ≥ 5% is flagged; one file per
  side therefore produces verdicts that are pure single-run noise. Use
  multiple seeds per side for decisions.
- **Δ% has no floor.** It is relative to the baseline mean, so near-zero
  baselines (e.g. `error_rate` ≈ 0.001) produce enormous percentages that
  always clear the 5% bar. Read them alongside the absolute Δ, which is shown
  next to them.

Per-metric aggregation only counts samples that actually contain the key, so
when the two groups mix export shapes (e.g. a `create-only` file next to
`create-delete` files) a row's effective n can be smaller than the group
header's sample count; the report prints a note under the table when that
happens and annotates affected conclusion rows with the reduced n.

### Network policies

| Policy | Create payload | What it exercises |
|---|---|---|
| `none` (default) | `templateID` only (+ optional `host-mount`) | Create without network rules |
| `rules` | `allow_internet_access=false` + ~24 `allowOut` (CIDR + domain) + 6 L7 `rules` (2 with inject) | Create with network rules: CubeVS allow/dns map updates and CubeEgress policy PUT |

`rules` uses a fixed built-in policy (stable fake hosts and dummy inject secrets). The bench only waits for create HTTP success; it does **not** validate dataplane allow/deny or in-guest connectivity.

Suggested A/B comparison (same `-c/-n/-t`, warm the pool once):

```bash
./bin/cube-bench -c 10 -n 50 -w 2 --network-policy none -o none.json
./bin/cube-bench -c 10 -n 50 -w 2 --network-policy rules -o rules.json
# Prefer Δ(rules − none) on create P50/P95 as the network-sensitive signal.
```

When comparing two Cube builds (for example pre/post network refactor), fix `--network-policy rules` and change only the server under test.

For `host-mount`, this CLI form is equivalent to the Python SDK pattern:

```python
metadata = {
    "host-mount": json.dumps([
        {"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": False},
    ])
}
```

`cube-bench` accepts the friendlier JSON array above, compacts it once, and
sends it as `metadata["host-mount"]` in the create request. The backend
contract still receives `metadata` as strings:

- `CubeAPI/src/services/sandboxes.rs` accepts `metadata` as `map[string]string`
- it lifts `metadata["host-mount"]` into the sandbox annotation `host-mount`
- `CubeMaster/pkg/service/sandbox/hostdir_mount.go` parses that annotation as
  a JSON string into mount descriptors

## Features

- Goroutine pool with configurable concurrency
- Scheduler benchmark workload generator: seeded Poisson arrivals, per-sandbox
  lifetimes (client-side DELETE + server-side `timeout` TTL), weighted
  multi-template mixes, and three presets (`burst`, `template_storm`,
  `mixed_spec`)
- Trace dump (`--dump-trace`) for replaying the exact same sequence in a
  scheduling simulator
- Live TUI dashboard: progress bar, real-time QPS, in-flight estimate, rolling
  operation log
- Final report: percentile table (P50/P95/P99), latency histogram, sparkline,
  queue-delay percentiles (scheduled mode), and letter grade (S/A/B/C/D)
- Built-in `--network-policy rules` mode for create-with-rules latency
- Dark/light/auto theme detection
- JSON report export (`-o report.json`)
- Dry-run mode for testing without a CubeSandbox server (scheduled mode is
  seed-reproducible in dry-run too)

## Clean up

```bash
make clean
```
