#!/usr/bin/env bash
# redis_key_migrate.sh - Redis key naming convention migration / cleanup tool.
#
# Usage:
#   redis_key_migrate.sh copy             Copy old keys to new keys WITHOUT
#                                         overwriting an existing (newer) new
#                                         key. Run before/around the dual phase
#                                         to speed up convergence.
#   redis_key_migrate.sh cleanup          Dry-run: list old keys that WOULD be
#                                         deleted, without deleting anything.
#   redis_key_migrate.sh cleanup --force  Actually delete the old keys. Run ONLY
#                                         after every service is on phase=new and
#                                         the new-key hit rate is verified.
#
# Connection env (all optional, sensible defaults):
#   REDIS_HOST (127.0.0.1) REDIS_PORT (6379) REDIS_DB (0) REDIS_PASSWORD ("")
#
# Notes:
#   - Uses SCAN (never KEYS) to avoid blocking the server.
#   - The password is passed via the REDISCLI_AUTH env var (read by redis-cli)
#     instead of "-a" so it does not leak into the process argument list.
#   - The legacy node-metric key is the bare nodeID with no fixed pattern, so it
#     is intentionally NOT handled here (it relies on heartbeat rebuild on the
#     new key + the new TTL on the old key expiring naturally).
set -euo pipefail

REDIS_CLI=(redis-cli -h "${REDIS_HOST:-127.0.0.1}" -p "${REDIS_PORT:-6379}" -n "${REDIS_DB:-0}")
# Pass auth via env so the secret never appears in `ps`/argv.
if [ -n "${REDIS_PASSWORD:-}" ]; then
  export REDISCLI_AUTH="${REDIS_PASSWORD}"
fi

ACTION="${1:-}"
FORCE="${2:-}"

# old key pattern -> new key prefix. The "*" suffix in the pattern marks the
# variable id portion that is carried over verbatim to the new key.
PATTERNS=(
  "bypass_host_proxy:*|cube:v1:shared:sandbox:proxy:"
  "cube_instance_info:*|cube:v1:master:instance:info:"
  "describetask:*|cube:v1:master:task:describe:"
)

scan_keys() {
  "${REDIS_CLI[@]}" --scan --pattern "$1"
}

copy_one() {
  local old_key="$1" new_prefix="$2" old_pattern="$3"
  local prefix="${old_pattern%\*}"        # pattern without trailing '*'
  local suffix="${old_key#"$prefix"}"     # the id portion
  local new_key="${new_prefix}${suffix}"
  # COPY without REPLACE: if the new key already exists (e.g. dual-phase writers
  # already populated a fresher value) COPY returns 0 and we leave it untouched,
  # so we never overwrite newer data with a stale legacy snapshot.
  local copied
  copied="$("${REDIS_CLI[@]}" COPY "$old_key" "$new_key")"
  if [ "$copied" = "1" ]; then
    local ttl
    ttl="$("${REDIS_CLI[@]}" TTL "$old_key")"
    if [ "$ttl" -gt 0 ]; then
      "${REDIS_CLI[@]}" EXPIRE "$new_key" "$ttl" >/dev/null
    fi
    echo "copied: $old_key -> $new_key (ttl=$ttl)"
  else
    echo "skipped (new key exists): $old_key -> $new_key"
  fi
}

do_copy() {
  local entry pat new_prefix k
  for entry in "${PATTERNS[@]}"; do
    pat="${entry%%|*}"
    new_prefix="${entry#*|}"
    while IFS= read -r k; do
      [ -n "$k" ] && copy_one "$k" "$new_prefix" "$pat"
    done < <(scan_keys "$pat")
  done
  echo "NOTE: bare nodeID metric keys are not copied (heartbeat rebuilds them)."
}

do_cleanup() {
  local dry_run=1
  if [ "${FORCE}" = "--force" ]; then
    dry_run=0
  fi
  local entry pat k count=0
  for entry in "${PATTERNS[@]}"; do
    pat="${entry%%|*}"
    while IFS= read -r k; do
      if [ -n "$k" ]; then
        count=$((count + 1))
        if [ "$dry_run" = "1" ]; then
          echo "[dry-run] would delete: $k"
        else
          "${REDIS_CLI[@]}" DEL "$k" >/dev/null
          echo "deleted: $k"
        fi
      fi
    done < <(scan_keys "$pat")
  done
  if [ "$dry_run" = "1" ]; then
    echo "[dry-run] ${count} key(s) matched. Re-run with: $0 cleanup --force"
  else
    echo "cleanup done, ${count} key(s) deleted."
  fi
  echo "NOTE: bare nodeID residue needs separate cleanup if required."
}

case "$ACTION" in
  copy) do_copy ;;
  cleanup) do_cleanup ;;
  *)
    echo "usage: $0 {copy|cleanup [--force]}" >&2
    exit 1
    ;;
esac
