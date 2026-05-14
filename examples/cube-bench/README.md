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
| `--api-url` | *(env)* | CubeAPI base URL |
| `--api-key` | *(env)* | API key |
| `--theme` | `auto` | Color theme: `dark`, `light`, or `auto` |
| `--dry-run` | `false` | Simulate API calls (no server needed) |
| `--dry-latency` | `80,30` | Dry-run latency: `mean,stddev` in ms |
| `--dry-error-rate` | `0.02` | Simulated error rate (0.0–1.0) |
| `--no-tui` | `false` | Disable interactive TUI |

### Examples

```bash
# Real server — 20 concurrent workers, 200 create+delete cycles
export E2B_API_URL=http://localhost:3000
export E2B_API_KEY=dummy
export CUBE_TEMPLATE_ID=<your-template-id>
./bin/cube-bench -c 20 -n 200

# Dry-run — no server required
./bin/cube-bench --dry-run -c 50 -n 500

# Create-only mode, export JSON report
./bin/cube-bench --dry-run -c 20 -n 200 -m create-only -o report.json

# Non-interactive output (CI / pipe)
./bin/cube-bench --dry-run --no-tui -c 10 -n 50

# Light terminal theme
./bin/cube-bench --dry-run --theme light -c 10 -n 100
```

## Features

- Goroutine pool with configurable concurrency
- Live TUI dashboard: progress bar, real-time QPS, rolling operation log
- Final report: percentile table (P50/P95/P99), latency histogram, sparkline,
  and letter grade (S/A/B/C/D)
- Dark/light/auto theme detection
- JSON report export (`-o report.json`)
- Dry-run mode for testing without a CubeSandbox server

## Clean up

```bash
make clean
```
