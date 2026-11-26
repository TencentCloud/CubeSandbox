#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for patch_int_key_in_section in both entrypoints.
# Covers commented defaults, live values, and missing sections.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_ENTRY="${SCRIPT_DIR}/cube-node-entrypoint.sh"
COMPONENT_ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "${got}" != "${want}" ]]; then
    printf 'FAIL: %s (got=%q want=%q)\n' "${msg}" "${got}" "${want}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

# Define the entrypoints' fail/log contract for the extracted helper.
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '%s\n' "$*"; }

# Load only the helper; sourcing the script would run main.
load_helper() { # <entrypoint>
  # shellcheck disable=SC1090
  source <(sed -n '/^patch_int_key_in_section()/,/^}/p' "$1")
  if ! declare -F patch_int_key_in_section >/dev/null; then
    printf 'FAIL: patch_int_key_in_section not found in %s\n' "$1" >&2
    exit 1
  fi
}

packaged_cfg() { # <path>
  cat >"$1" <<'TOML'
[plugins]
  [plugins."io.cubelet.internal.v1.cgroup"]
    auto_pool_size = true
    # pool_size = 3000
    pool_workers = 1

  [plugins."io.cubelet.internal.v1.network"]
    auto_tap_init_num = true
    # tap_init_num = 500
    cidr = "192.168.0.0/18"

  [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    # pool_size = 3000
    # pool_worker_num = 8
TOML
}

live_cfg() { # <path>
  cat >"$1" <<'TOML'
[plugins]
  [plugins."io.cubelet.internal.v1.cgroup"]
    auto_pool_size = true
    pool_size = 3000
    pool_workers = 1

  [plugins."io.cubelet.internal.v1.network"]
    auto_tap_init_num = true
    tap_init_num = 500
    cidr = "192.168.0.0/18"
TOML
}

# Count matches within one section.
count_in_section() { # <cfg> <section> <regex>
  awk -v header="[plugins.\"$2\"]" -v re="$3" '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    { t = trim($0) }
    t == header { in_section = 1; next }
    in_section && substr(t, 1, 1) == "[" { in_section = 0 }
    in_section && t ~ re { n++ }
    END { print n + 0 }
  ' "$1"
}

run_suite() { # <label> <entrypoint>
  local label="$1" entry="$2"
  local cfg

  load_helper "${entry}"

  # --- commented defaults ---
  cfg="${TMP}/${label}-packaged.toml"
  packaged_cfg "${cfg}"
  patch_int_key_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" "pool_size" 1800
  patch_int_key_in_section "${cfg}" "io.cubelet.internal.v1.network" "tap_init_num" 0

  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" '^pool_size = 1800$')" "1" \
    "${label}: cgroup pool_size written into its section"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" '^# pool_size')" "0" \
    "${label}: commented cgroup pool_size consumed, not duplicated"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.network" '^tap_init_num = 0$')" "1" \
    "${label}: tap_init_num written into its section"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.network" '^# tap_init_num')" "0" \
    "${label}: commented tap_init_num consumed, not duplicated"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.storage" '^# pool_size = 3000$')" "1" \
    "${label}: storage's commented pool_size left untouched"
  assert_eq "$(grep -c 'auto_pool_size = true' "${cfg}")" "1" \
    "${label}: auto switch preserved"
  # Validate TOML when python3 is available.
  if command -v python3 >/dev/null 2>&1; then
    python3 - "${cfg}" <<'PY'
import sys
try:
    import tomllib
except ImportError:
    sys.exit(0)
with open(sys.argv[1], "rb") as f:
    tomllib.load(f)
PY
    printf 'ok: %s: patched packaged config is valid TOML\n' "${label}"
  fi

  # --- live values ---
  cfg="${TMP}/${label}-live.toml"
  live_cfg "${cfg}"
  patch_int_key_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" "pool_size" 42
  patch_int_key_in_section "${cfg}" "io.cubelet.internal.v1.network" "tap_init_num" 7

  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" '^pool_size = 42$')" "1" \
    "${label}: live pool_size replaced in place"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" '^pool_size = 3000$')" "0" \
    "${label}: live pool_size not duplicated"
  assert_eq "$(count_in_section "${cfg}" "io.cubelet.internal.v1.network" '^tap_init_num = 7$')" "1" \
    "${label}: live tap_init_num replaced in place"

  # --- missing section ---
  cfg="${TMP}/${label}-missing.toml"
  cat >"${cfg}" <<'TOML'
[plugins."io.cubelet.internal.v1.network"]
  tap_init_num = 500
TOML
  cp "${cfg}" "${TMP}/${label}-missing.expected"
  if ( patch_int_key_in_section "${cfg}" "io.cubelet.internal.v1.cgroup" "pool_size" 5 ) 2>/dev/null; then
    printf 'FAIL: %s: expected failure when the target section is absent\n' "${label}" >&2
    exit 1
  fi
  printf 'ok: %s: absent section is rejected\n' "${label}"

  # Failed patch leaves the file untouched.
  assert_eq "$(cat "${cfg}")" "$(cat "${TMP}/${label}-missing.expected")" \
    "${label}: failed patch left the file untouched"
}

run_suite "node" "${NODE_ENTRY}"
run_suite "component" "${COMPONENT_ENTRY}"

printf 'all patch_int_key_in_section checks passed\n'
