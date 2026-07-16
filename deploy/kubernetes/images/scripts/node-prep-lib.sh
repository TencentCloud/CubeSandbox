#!/bin/sh
# Shared helpers for node-prep-ready sentinel (Big Pod REV3.2).
# Source from pvm-host-bootstrap / cube-node-init / write-node-prep-ready / wait-node-prep.
#
# Fingerprint fields (version=1):
#   version, mode (full|noop), kernel, boot_args (space-separated required tokens),
#   prep_generation, pvm_enabled, node_init_enabled

STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
HOST_ROOT="${HOST_ROOT:-}"
NODE_PREP_READY_NAME="${NODE_PREP_READY_NAME:-node-prep-ready}"
PREP_GENERATION="${PREP_GENERATION:-1}"
PVM_ENABLED="${PVM_ENABLED:-0}"
NODE_INIT_ENABLED="${NODE_INIT_ENABLED:-0}"
PREP_MODE="${PREP_MODE:-}"
KERNEL_BOOT_ARGS="${KERNEL_BOOT_ARGS:-${PVM_KERNEL_BOOT_ARGS:-}}"
DESIRED_KERNEL_PATTERN="${DESIRED_KERNEL_PATTERN:-}"

node_prep_host_path() {
  if [ -n "$HOST_ROOT" ] && [ "$HOST_ROOT" != "/" ]; then
    printf '%s%s' "$HOST_ROOT" "$1"
  else
    printf '%s' "$1"
  fi
}

node_prep_state_dir() {
  node_prep_host_path "$STATE_DIR"
}

node_prep_ready_path() {
  node_prep_host_path "${STATE_DIR}/${NODE_PREP_READY_NAME}"
}

node_prep_resolve_mode() {
  if [ -n "$PREP_MODE" ]; then
    printf '%s' "$PREP_MODE"
    return 0
  fi
  if [ "$PVM_ENABLED" != "1" ] && [ "$PVM_ENABLED" != "true" ] \
    && [ "$NODE_INIT_ENABLED" != "1" ] && [ "$NODE_INIT_ENABLED" != "true" ]; then
    printf 'noop'
  else
    printf 'full'
  fi
}

node_prep_bool01() {
  case "$1" in
    1|true|TRUE|yes|YES) printf '1' ;;
    *) printf '0' ;;
  esac
}

node_prep_missing_boot_args() {
  [ -n "$KERNEL_BOOT_ARGS" ] || return 0
  cmdline=" $(cat /proc/cmdline 2>/dev/null || true) "
  missing=""
  for arg in $KERNEL_BOOT_ARGS; do
    case "$cmdline" in
      *" ${arg} "*) ;;
      *) missing="${missing}${missing:+ }${arg}" ;;
    esac
  done
  [ -z "$missing" ] || printf '%s' "$missing"
}

node_prep_compute_fingerprint() {
  mode="$(node_prep_resolve_mode)"
  kernel="$(uname -r 2>/dev/null || true)"
  pvm="$(node_prep_bool01 "$PVM_ENABLED")"
  ninit="$(node_prep_bool01 "$NODE_INIT_ENABLED")"
  boot_args=""
  if [ "$mode" = "full" ] && [ "$pvm" = "1" ]; then
    boot_args="$(printf '%s' "$KERNEL_BOOT_ARGS" | tr -s ' ' | sed 's/^ //;s/ $//')"
  fi
  printf 'version=1\n'
  printf 'mode=%s\n' "$mode"
  printf 'kernel=%s\n' "$kernel"
  printf 'boot_args=%s\n' "$boot_args"
  printf 'prep_generation=%s\n' "$PREP_GENERATION"
  printf 'pvm_enabled=%s\n' "$pvm"
  printf 'node_init_enabled=%s\n' "$ninit"
}

node_prep_fingerprint_matches_file() {
  ready="$(node_prep_ready_path)"
  [ -f "$ready" ] || return 1
  expected="$(node_prep_compute_fingerprint)"
  actual="$(cat "$ready")"
  [ "$expected" = "$actual" ]
}

node_prep_fingerprint_valid_for_wait() {
  # Wait accepts a ready file whose fields match the currently desired fingerprint.
  node_prep_fingerprint_matches_file
}

invalidate_node_prep_ready() {
  ready="$(node_prep_ready_path)"
  if [ -e "$ready" ] || [ -e "${ready}.tmp" ]; then
    rm -f "$ready" "${ready}.tmp"
    printf '[node-prep] invalidated %s\n' "$ready"
  fi
}

write_node_prep_ready() {
  dir="$(node_prep_state_dir)"
  mkdir -p "$dir"
  ready="$(node_prep_ready_path)"
  tmp="${ready}.tmp"
  node_prep_compute_fingerprint > "$tmp"
  mv -f "$tmp" "$ready"
  printf '[node-prep] wrote %s\n' "$ready"
}

node_prep_kernel_ready() {
  # True when running kernel already satisfies pvm pattern + required boot args
  # (or pvm is disabled).
  pvm="$(node_prep_bool01 "$PVM_ENABLED")"
  [ "$pvm" = "1" ] || return 0
  [ -n "$DESIRED_KERNEL_PATTERN" ] || return 1
  current_kernel="$(uname -r 2>/dev/null || true)"
  printf '%s' "$current_kernel" | grep -q "$DESIRED_KERNEL_PATTERN" || return 1
  missing="$(node_prep_missing_boot_args)"
  [ -z "$missing" ]
}
