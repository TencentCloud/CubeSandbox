#!/usr/bin/env bash
# Sequential sandbox create -> port ready through Cube Proxy -> delete.
#   bench.sh TEMPLATE_ID [COUNT=10] [PORT=49983] [PATH=/health]
# Runs in the VM (or on the Mac with API=http://127.0.0.1:13000 PROXY=http://127.0.0.1:18080).
set -uo pipefail
tpl=$1; n=${2:-10}; port=${3:-49983}; path=${4:-/health}
api=${API:-http://localhost:3000}; proxy=${PROXY:-http://127.0.0.1}; domain=${DOMAIN:-cube.app}
export no_proxy='*'
for _ in $(seq 1 "$n"); do
  out=$(curl -sS -m 180 -X POST "$api/sandboxes" -H 'Content-Type: application/json' \
    -d "{\"templateID\":\"$tpl\",\"timeout\":300}" -w ' %{time_total}')
  create=${out##* }
  id=$(grep -o '"sandboxID":"[^"]*' <<<"$out" | cut -d'"' -f4)
  [[ -n "$id" ]] || { echo "create failed: ${out:0:300}"; continue; }
  start=$(date +%s.%N)
  for _ in $(seq 1 600); do
    [[ "$(curl -s -o /dev/null -m 2 -w '%{http_code}' -H "Host: $port-$id.$domain" "$proxy$path")" =~ ^2 ]] && break
    sleep 0.05
  done
  ready=$(echo "$(date +%s.%N) - $start" | bc)
  del=$(curl -s -X DELETE "$api/sandboxes/$id" -o /dev/null -w '%{http_code} %{time_total}')
  printf 'create=%ss ready_after=%.3fs delete=%s\n' "$create" "$ready" "$del"
done
