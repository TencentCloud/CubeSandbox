#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/guest_workload_task_stats.sh"
tmp_dir="$(mktemp -d)"
socket_pid=""
cleanup() {
  if [[ -n "$socket_pid" ]]; then
    kill "$socket_pid" 2>/dev/null || true
    wait "$socket_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

assert_contains() {
  local output="$1"
  local expected="$2"
  if [[ "$output" != *"$expected"* ]]; then
    echo "expected output to contain: $expected" >&2
    echo "$output" >&2
    exit 1
  fi
}

skip_output="$(env -u CUBECLI -u CUBELET_ADDRESS "$script")"
assert_contains "$skip_output" "SKIP:"

cat >"$tmp_dir/cubecli" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" != "--address" || "$2" != "$CUBELET_ADDRESS" ]]; then
  echo "unexpected Cubelet address arguments: $*" >&2
  exit 1
fi
printf '%s\n' '{"data":{"cpu":{"usage":{"total":2000000,"user":1200000,"kernel":800000}},"memory":{"usage":{"usage":4096,"limit":536870912}}}}'
EOF
chmod +x "$tmp_dir/cubecli"

skip_without_socket_output="$(CUBECLI="$tmp_dir/cubecli" env -u CUBELET_ADDRESS "$script")"
assert_contains "$skip_without_socket_output" "SKIP:"

set +e
missing_socket_output="$(CUBECLI="$tmp_dir/cubecli" CUBELET_ADDRESS="$tmp_dir/missing.sock" "$script" sandbox-id 2>&1)"
missing_socket_status=$?
set -e
if [[ $missing_socket_status -ne 2 ]]; then
  echo "expected explicit missing CUBELET_ADDRESS to exit 2, got $missing_socket_status" >&2
  exit 1
fi
assert_contains "$missing_socket_output" "Cubelet socket is not available"

python3 - "$tmp_dir/cubelet.sock" "$tmp_dir/socket-ready" <<'PY' &
import signal
import socket
import sys

server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(sys.argv[1])
open(sys.argv[2], "w", encoding="utf-8").close()
signal.pause()
PY
socket_pid=$!
for _ in {1..50}; do
  [[ -e "$tmp_dir/socket-ready" ]] && break
  sleep 0.02
done
if [[ ! -S "$tmp_dir/cubelet.sock" ]]; then
  echo "failed to create test Cubelet socket" >&2
  exit 1
fi

set +e
missing_output="$(CUBECLI="$tmp_dir/missing-cubecli" CUBELET_ADDRESS="$tmp_dir/cubelet.sock" "$script" sandbox-id 2>&1)"
missing_status=$?
set -e
if [[ $missing_status -ne 2 ]]; then
  echo "expected explicit missing CUBECLI to exit 2, got $missing_status" >&2
  exit 1
fi
assert_contains "$missing_output" "cubecli is not executable"

pass_output="$(CUBECLI="$tmp_dir/cubecli" CUBELET_ADDRESS="$tmp_dir/cubelet.sock" "$script" sandbox-id)"
assert_contains "$pass_output" '"result": "PASS"'

echo "guest_workload_task_stats tests: PASS"
