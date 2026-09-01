#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the two awk key-guarantee patchers in
# patch_cubemaster_external_deps (install.sh): they must idempotently ensure
# `instance_db_config` carries a `driver:` line and, for the postgres driver, a
# `postgres:` sub-block with `sslmode:` — without ever emitting a duplicate key
# (gopkg.in/yaml.v3 rejects duplicate mapping keys, so a dup would make
# CubeMaster fail to load conf.yaml at boot).
#
# The awk programs are extracted verbatim from install.sh rather than
# reimplemented, so the tests exercise the shipped code. install.sh itself is
# not sourceable (it runs its installer main body and requires root/KVM).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
INSTALL_SH="${ONE_CLICK_DIR}/install.sh"

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "${TMP_DIR}"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Extract the awk program body of the Nth `awk '...'` invocation whose program
# is terminated by `' "${cfg}" > "${cfg}.tmp"`. Prints just the awk source.
extract_awk() {
  local nth="$1" file="$2"
  awk -v want="${nth}" '
    !inprog && /awk '\''/ { n++; if (n==want) { inprog=1; next } }
    inprog && /^[[:space:]]*'\''[[:space:]]+"\$\{cfg\}"/ { inprog=0; exit }
    inprog { print }
  ' "${file}"
}

AWK_DRIVER="$(extract_awk 1 "${INSTALL_SH}")"
AWK_POSTGRES="$(extract_awk 2 "${INSTALL_SH}")"
[[ -n "${AWK_DRIVER}" ]] || fail "could not extract driver-guarantee awk from install.sh"
[[ -n "${AWK_POSTGRES}" ]] || fail "could not extract postgres-guarantee awk from install.sh"

run_driver() { awk "${AWK_DRIVER}" "$1"; }
run_postgres() { awk "${AWK_POSTGRES}" "$1"; }

# Assert a key appears exactly `want` times in a rendered config.
assert_count() {
  local label="$1" key="$2" want="$3" file="$4" got
  got="$(grep -c "${key}" "${file}" || true)"
  [[ "${got}" -eq "${want}" ]] \
    || fail "${label}: expected ${want}x '${key}', got ${got} in:
$(cat "${file}")"
}

write_cfg() { printf '%s' "$1" > "$2"; }

# --- driver-guarantee awk -----------------------------------------------------

test_driver_inserted_when_missing() {
  local in="${TMP_DIR}/d1.in" out="${TMP_DIR}/d1.out"
  write_cfg $'common:\n  cube_ops_addr: "http://127.0.0.1:3010"\ninstance_db_config:\n  addr: "127.0.0.1:3306"\n  user: "cube"\nredis:\n  nodes: "127.0.0.1:6379"\n' "${in}"
  run_driver "${in}" > "${out}"
  assert_count "driver-missing" '^  driver:' 1 "${out}"
  grep -q '  driver: "mysql"' "${out}" || fail "driver-missing: default driver should be mysql"
  # Must not touch the common: block.
  grep -q '  cube_ops_addr: "http://127.0.0.1:3010"' "${out}" \
    || fail "driver-missing: cube_ops_addr clobbered"
}

test_driver_idempotent_when_present() {
  local in="${TMP_DIR}/d2.in" out="${TMP_DIR}/d2.out"
  write_cfg $'instance_db_config:\n  driver: "postgres"\n  addr: "127.0.0.1:5432"\nredis:\n  nodes: "x"\n' "${in}"
  run_driver "${in}" > "${out}"
  assert_count "driver-present" '^  driver:' 1 "${out}"
  grep -q '  driver: "postgres"' "${out}" \
    || fail "driver-present: existing driver value must be preserved"
}

# --- postgres/sslmode-guarantee awk -------------------------------------------

test_postgres_block_appended_when_absent() {
  local in="${TMP_DIR}/p1.in" out="${TMP_DIR}/p1.out"
  write_cfg $'instance_db_config:\n  driver: "postgres"\n  addr: "10.0.0.9:5432"\nredis:\n  nodes: "x"\n' "${in}"
  run_postgres "${in}" > "${out}"
  assert_count "pg-absent" '^  postgres:' 1 "${out}"
  assert_count "pg-absent" '^    sslmode:' 1 "${out}"
}

test_postgres_block_at_eof() {
  local in="${TMP_DIR}/p2.in" out="${TMP_DIR}/p2.out"
  write_cfg $'redis:\n  nodes: "x"\ninstance_db_config:\n  driver: "postgres"\n  addr: "10.0.0.9:5432"\n' "${in}"
  run_postgres "${in}" > "${out}"
  assert_count "pg-eof" '^  postgres:' 1 "${out}"
  assert_count "pg-eof" '^    sslmode:' 1 "${out}"
}

test_sslmode_inserted_into_existing_postgres() {
  local in="${TMP_DIR}/p3.in" out="${TMP_DIR}/p3.out"
  write_cfg $'instance_db_config:\n  driver: "postgres"\n  postgres:\n    connect_timeout: "5"\nredis:\n  nodes: "x"\n' "${in}"
  run_postgres "${in}" > "${out}"
  assert_count "pg-nosslmode" '^  postgres:' 1 "${out}"
  assert_count "pg-nosslmode" '^    sslmode:' 1 "${out}"
  grep -q '    connect_timeout: "5"' "${out}" \
    || fail "pg-nosslmode: existing sub-block key must be preserved"
}

test_sslmode_idempotent_when_present() {
  local in="${TMP_DIR}/p4.in" out="${TMP_DIR}/p4.out"
  write_cfg $'instance_db_config:\n  driver: "postgres"\n  postgres:\n    sslmode: "require"\nredis:\n  nodes: "x"\n' "${in}"
  run_postgres "${in}" > "${out}"
  assert_count "pg-sslmode-present" '^    sslmode:' 1 "${out}"
  grep -q '    sslmode: "require"' "${out}" \
    || fail "pg-sslmode-present: existing sslmode value must be preserved"
}

# Regression: a blank line inside the postgres: sub-block (as a hand edit can
# introduce) must NOT be treated as the end of the block. Without the blank-line
# guard the scanner decides sslmode is missing and injects a duplicate, which
# yaml.v3 then rejects at boot.
test_blank_line_in_postgres_block_no_duplicate() {
  local in="${TMP_DIR}/p5.in" out="${TMP_DIR}/p5.out"
  write_cfg $'instance_db_config:\n  driver: "postgres"\n  postgres:\n\n    sslmode: "disable"\nredis:\n  nodes: "x"\n' "${in}"
  run_postgres "${in}" > "${out}"
  assert_count "pg-blankline" '^    sslmode:' 1 "${out}"
  assert_count "pg-blankline" '^  postgres:' 1 "${out}"
}

test_driver_inserted_when_missing
test_driver_idempotent_when_present
test_postgres_block_appended_when_absent
test_postgres_block_at_eof
test_sslmode_inserted_into_existing_postgres
test_sslmode_idempotent_when_present
test_blank_line_in_postgres_block_no_duplicate

echo "conf.yaml db patch tests OK"
