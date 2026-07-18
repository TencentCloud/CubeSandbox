#!/bin/sh
# Shared helpers for node-prep / pvm-host sentinels (Big Pod + PVM DS).
# Source from pvm-host-bootstrap / wait-pvm-host / cube-node-init /
# write-node-prep-ready / wait-node-prep.
#
# Fingerprint fields for node-prep-ready (version=1):
#   version, mode (full|noop), kernel, boot_args (space-separated required tokens),
#   prep_generation, pvm_enabled, node_init_enabled
#
# Fingerprint fields for pvm-host-ready (version=1):
#   version, kernel, boot_args, desired_pattern
# Presence alone is NEVER enough — waiters compare against live uname/cmdline.
#
# pvm-mutating: set while host kernel/bootloader mutation is in progress so
# waiters fail closed across the invalidate → reboot window.

STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"
HOST_ROOT="${HOST_ROOT:-}"
NODE_PREP_READY_NAME="${NODE_PREP_READY_NAME:-node-prep-ready}"
PVM_HOST_READY_NAME="${PVM_HOST_READY_NAME:-pvm-host-ready}"
EFFECTIVE_PVM_NAME="${EFFECTIVE_PVM_NAME:-effective-pvm}"
PVM_MUTATING_NAME="${PVM_MUTATING_NAME:-pvm-mutating}"
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

pvm_host_ready_path() {
  node_prep_host_path "${STATE_DIR}/${PVM_HOST_READY_NAME}"
}

effective_pvm_path() {
  node_prep_host_path "${STATE_DIR}/${EFFECTIVE_PVM_NAME}"
}

pvm_mutating_path() {
  node_prep_host_path "${STATE_DIR}/${PVM_MUTATING_NAME}"
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

node_prep_normalize_boot_args() {
  printf '%s' "$KERNEL_BOOT_ARGS" | tr -s ' ' | sed 's/^ //;s/ $//'
}

# Prefer per-node effective-pvm (written by wait-pvm-host) over Helm env.
apply_effective_pvm_env() {
  path="$(effective_pvm_path)"
  [ -f "$path" ] || return 0
  val="$(tr -d '[:space:]' < "$path" 2>/dev/null || true)"
  case "$val" in
    0|1)
      PVM_ENABLED="$val"
      CUBE_PVM_ENABLE="$val"
      export PVM_ENABLED CUBE_PVM_ENABLE
      ;;
  esac
}

write_effective_pvm() {
  val="$(node_prep_bool01 "$1")"
  dir="$(node_prep_state_dir)"
  mkdir -p "$dir"
  path="$(effective_pvm_path)"
  tmp="${path}.tmp"
  printf '%s\n' "$val" > "$tmp"
  mv -f "$tmp" "$path"
  PVM_ENABLED="$val"
  CUBE_PVM_ENABLE="$val"
  export PVM_ENABLED CUBE_PVM_ENABLE
  printf '[node-prep] wrote %s=%s\n' "$path" "$val"
}

mark_pvm_mutating() {
  dir="$(node_prep_state_dir)"
  mkdir -p "$dir"
  path="$(pvm_mutating_path)"
  tmp="${path}.tmp"
  printf '1\n' > "$tmp"
  mv -f "$tmp" "$path"
  printf '[node-prep] marked %s\n' "$path"
}

clear_pvm_mutating() {
  path="$(pvm_mutating_path)"
  if [ -e "$path" ] || [ -e "${path}.tmp" ]; then
    rm -f "$path" "${path}.tmp"
    printf '[node-prep] cleared %s\n' "$path"
  fi
}

pvm_is_mutating() {
  [ -f "$(pvm_mutating_path)" ]
}

node_prep_compute_fingerprint() {
  apply_effective_pvm_env
  mode="$(node_prep_resolve_mode)"
  kernel="$(uname -r 2>/dev/null || true)"
  pvm="$(node_prep_bool01 "$PVM_ENABLED")"
  ninit="$(node_prep_bool01 "$NODE_INIT_ENABLED")"
  boot_args=""
  if [ "$mode" = "full" ] && [ "$pvm" = "1" ]; then
    boot_args="$(node_prep_normalize_boot_args)"
  fi
  printf 'version=1\n'
  printf 'mode=%s\n' "$mode"
  printf 'kernel=%s\n' "$kernel"
  printf 'boot_args=%s\n' "$boot_args"
  printf 'prep_generation=%s\n' "$PREP_GENERATION"
  printf 'pvm_enabled=%s\n' "$pvm"
  printf 'node_init_enabled=%s\n' "$ninit"
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

pvm_host_compute_fingerprint() {
  kernel="$(uname -r 2>/dev/null || true)"
  boot_args="$(node_prep_normalize_boot_args)"
  printf 'version=1\n'
  printf 'kernel=%s\n' "$kernel"
  printf 'boot_args=%s\n' "$boot_args"
  printf 'desired_pattern=%s\n' "$DESIRED_KERNEL_PATTERN"
}

pvm_host_fingerprint_matches_file() {
  # File must exist AND match what live uname/cmdline would produce now.
  # Fail closed while mutation is in progress.
  pvm_is_mutating && return 1
  ready="$(pvm_host_ready_path)"
  [ -f "$ready" ] || return 1
  node_prep_kernel_ready || return 1
  expected="$(pvm_host_compute_fingerprint)"
  actual="$(cat "$ready")"
  [ "$expected" = "$actual" ]
}

write_pvm_host_ready() {
  # Only call after live kernel already satisfies pattern + boot args.
  if ! node_prep_kernel_ready; then
    printf '[node-prep] ERROR: refusing to write pvm-host-ready; live kernel not ready\n' >&2
    return 1
  fi
  dir="$(node_prep_state_dir)"
  mkdir -p "$dir"
  ready="$(pvm_host_ready_path)"
  tmp="${ready}.tmp"
  pvm_host_compute_fingerprint > "$tmp"
  mv -f "$tmp" "$ready"
  clear_pvm_mutating
  printf '[node-prep] wrote %s\n' "$ready"
}

node_prep_fingerprint_matches_file() {
  ready="$(node_prep_ready_path)"
  [ -f "$ready" ] || return 1
  expected="$(node_prep_compute_fingerprint)"
  actual="$(cat "$ready")"
  [ "$expected" = "$actual" ] || return 1
  # When this node is in PVM mode, also require live PVM host readiness.
  # Prevents accepting a self-consistent but wrong-kernel node-prep-ready.
  pvm="$(node_prep_bool01 "$PVM_ENABLED")"
  if [ "$pvm" = "1" ]; then
    pvm_is_mutating && return 1
    node_prep_kernel_ready || return 1
    pvm_host_fingerprint_matches_file || return 1
  fi
  return 0
}

invalidate_node_prep_ready() {
  ready="$(node_prep_ready_path)"
  if [ -e "$ready" ] || [ -e "${ready}.tmp" ]; then
    rm -f "$ready" "${ready}.tmp"
    printf '[node-prep] invalidated %s\n' "$ready"
  fi
}

write_node_prep_ready() {
  apply_effective_pvm_env
  pvm="$(node_prep_bool01 "$PVM_ENABLED")"
  if [ "$pvm" = "1" ]; then
    if pvm_is_mutating; then
      printf '[node-prep] ERROR: refusing to write node-prep-ready while pvm-mutating\n' >&2
      return 1
    fi
    if ! node_prep_kernel_ready; then
      printf '[node-prep] ERROR: refusing to write node-prep-ready; live PVM kernel not ready\n' >&2
      return 1
    fi
    if ! pvm_host_fingerprint_matches_file; then
      printf '[node-prep] ERROR: refusing to write node-prep-ready; pvm-host-ready fingerprint mismatch\n' >&2
      return 1
    fi
  fi
  dir="$(node_prep_state_dir)"
  mkdir -p "$dir"
  ready="$(node_prep_ready_path)"
  tmp="${ready}.tmp"
  # Write fingerprint directly (do not capture via $(); trailing newlines matter).
  node_prep_compute_fingerprint > "$tmp"
  mv -f "$tmp" "$ready"
  printf '[node-prep] wrote %s\n' "$ready"
}

invalidate_pvm_host_ready() {
  ready="$(pvm_host_ready_path)"
  if [ -e "$ready" ] || [ -e "${ready}.tmp" ]; then
    rm -f "$ready" "${ready}.tmp"
    printf '[node-prep] invalidated %s\n' "$ready"
  fi
}

invalidate_effective_pvm() {
  path="$(effective_pvm_path)"
  if [ -e "$path" ] || [ -e "${path}.tmp" ]; then
    rm -f "$path" "${path}.tmp"
    printf '[node-prep] invalidated %s\n' "$path"
  fi
}

# Call before any host mutate that may reboot (PVM install / boot-args change).
invalidate_pvm_gate_sentinels() {
  mark_pvm_mutating
  invalidate_node_prep_ready
  invalidate_pvm_host_ready
  invalidate_effective_pvm
}
