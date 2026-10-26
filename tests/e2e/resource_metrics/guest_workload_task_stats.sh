#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${CUBECLI:-}" ]]; then
  echo "SKIP: set CUBECLI and CUBELET_ADDRESS to run the node-local Task.Stats validation"
  exit 0
fi

if [[ -z "${CUBELET_ADDRESS:-}" ]]; then
  echo "SKIP: set CUBECLI and CUBELET_ADDRESS to run the node-local Task.Stats validation"
  exit 0
fi

if [[ ! -x "$CUBECLI" ]]; then
  echo "cubecli is not executable: $CUBECLI" >&2
  exit 2
fi

if [[ ! -S "$CUBELET_ADDRESS" ]]; then
  echo "Cubelet socket is not available: $CUBELET_ADDRESS" >&2
  exit 2
fi

sandbox_id="${1:-}"
if [[ -z "$sandbox_id" ]]; then
  echo "usage: $0 <sandbox-id>" >&2
  exit 2
fi

output="$($CUBECLI --address "$CUBELET_ADDRESS" containerd tasks metrics --format json "$sandbox_id")"
if [[ -z "$output" ]]; then
  echo "Task.Stats returned no metrics for sandbox $sandbox_id" >&2
  exit 1
fi

printf '%s\n' "$output"

METRICS_JSON="$output" python3 - "$sandbox_id" <<'PY'
import json
import os
import sys

sandbox_id = sys.argv[1]
raw = os.environ["METRICS_JSON"].strip()
try:
    metric = json.loads(raw)
except json.JSONDecodeError as exc:
    raise SystemExit(f"cubecli metrics output is not JSON: {exc}")

data = metric.get("data", metric)
cpu = data.get("cpu", {}).get("usage", {})
memory = data.get("memory", {}).get("usage", {})

total = int(cpu.get("total", 0))
user = int(cpu.get("user", 0))
kernel = int(cpu.get("kernel", 0))
current = int(memory.get("usage", 0))
limit = int(memory.get("limit", 0))

if total <= 0 or user + kernel <= 0:
    raise SystemExit(f"CPU counters are missing for {sandbox_id}: {cpu}")
if total < 1_000_000:
    raise SystemExit(
        f"CPU total is too small for a nanosecond counter: {total}; "
        "check cgroup v2 microsecond-to-nanosecond normalization"
    )
if current <= 0 or limit <= 0:
    raise SystemExit(f"memory usage or limit is missing for {sandbox_id}: {memory}")

print(
    json.dumps(
        {
            "sandbox_id": sandbox_id,
            "cpu_total_ns": total,
            "cpu_user_ns": user,
            "cpu_system_ns": kernel,
            "memory_current_bytes": current,
            "memory_limit_bytes": limit,
            "result": "PASS",
        },
        sort_keys=True,
    )
)
PY
