#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cloud-free tests for uninstall.sh's removal of s3lvol write-back rings.
#
# The rings live in /dev/shm and survive an uninstall unless it removes them.
# One that still holds records makes the next install's lvstore create refuse,
# the target exit at every start, and the install time out after 600s waiting
# for the RPC socket -- with nothing in its output pointing at /dev/shm. These
# run the real function against a scratch directory (S3LVOL_SHM_DIR), covering
# both ring layouts, a ring held by a live process, and the leftover check.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNINSTALL="$(cd "${SCRIPT_DIR}/../scripts/one-click" && pwd)/uninstall.sh"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

command -v flock >/dev/null 2>&1 || { echo "SKIP: flock not available"; exit 0; }

# uninstall.sh runs its command dispatch at the bottom, so it cannot be sourced.
# Pull out just the functions under test.
warn() { printf 'WARN: %s\n' "$*" >&2; }
eval "$(sed -n '/^remove_s3lvol_writeback_rings()/,/^}/p' "${UNINSTALL}")"
declare -F remove_s3lvol_writeback_rings >/dev/null \
  || { echo "FAIL: remove_s3lvol_writeback_rings() not found in ${UNINSTALL}" >&2; exit 1; }

tmp="$(mktemp -d)"
holder=""
cleanup() {
  [[ -n "${holder}" ]] && kill "${holder}" 2>/dev/null || true
  rm -rf "${tmp}"
}
trap cleanup EXIT

export S3LVOL_SHM_DIR="${tmp}/shm"
# Pinned away from the real /data/cubelet/rcow so the guard in
# remove_s3lvol_writeback_rings() is exercised against this scratch tree
# rather than whatever the host happens to have installed.
export S3LVOL_STATE_DIR="${tmp}/state"

reset_shm() {
  rm -rf "${S3LVOL_SHM_DIR}"
  mkdir -p "${S3LVOL_SHM_DIR}/s3lvol-wb-0"
}

test_removes_both_layouts() {
  reset_shm
  # Current layout: a per-uid directory with one ring per lvstore.
  : >"${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-0123456789abcdef0123456789abcdef"
  : >"${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-fedcba9876543210fedcba9876543210"
  # Older builds put the ring straight in /dev/shm.
  : >"${S3LVOL_SHM_DIR}/s3lvol-wb-00000000000000000000000000000001"
  # Something unrelated must be left alone.
  : >"${S3LVOL_SHM_DIR}/other-app-segment"

  remove_s3lvol_writeback_rings 2>/dev/null

  [[ -z "$(compgen -G "${S3LVOL_SHM_DIR}/s3lvol-wb-*" || true)" ]] \
    || fail "rings or their directory left behind: $(ls -A "${S3LVOL_SHM_DIR}")"
  [[ -e "${S3LVOL_SHM_DIR}/other-app-segment" ]] \
    || fail "an unrelated /dev/shm entry was removed"
}

test_leaves_a_held_ring() {
  reset_shm
  local held="${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-11111111111111111111111111111111"
  local free="${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-22222222222222222222222222222222"
  : >"${held}"
  : >"${free}"

  # Stands in for a target that is still running: s3_writeback_open() holds an
  # exclusive flock on the ring for as long as the process lives.
  # exec, so the pid in $! is the one holding the lock. With `flock FILE cmd`
  # the fd is inherited by cmd and outlives a kill of flock itself.
  ( exec 9<>"${held}"; flock -x 9; exec sleep 30 ) &
  holder=$!
  local _
  for _ in $(seq 50); do
    flock -n "${held}" true 2>/dev/null || break
    sleep 0.1
  done

  local out
  out="$(remove_s3lvol_writeback_rings 2>&1)"

  [[ -e "${held}" ]] || fail "a ring held by a live process was removed"
  [[ ! -e "${free}" ]] || fail "the unheld ring next to it was not removed"
  [[ "${out}" == *"held by a running process"* ]] \
    || fail "no warning for the held ring: ${out}"
  [[ -d "${S3LVOL_SHM_DIR}/s3lvol-wb-0" ]] \
    || fail "the directory of a held ring was removed"

  kill "${holder}" 2>/dev/null || true
  wait "${holder}" 2>/dev/null || true
  holder=""
}

test_skips_symlinks() {
  reset_shm
  local victim="${tmp}/victim"
  : >"${victim}"
  ln -s "${victim}" "${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-33333333333333333333333333333333"

  remove_s3lvol_writeback_rings 2>/dev/null

  [[ -e "${victim}" ]] || fail "a symlink target outside /dev/shm was removed"
}

test_empty_shm_is_fine() {
  rm -rf "${S3LVOL_SHM_DIR}"
  mkdir -p "${S3LVOL_SHM_DIR}"
  remove_s3lvol_writeback_rings 2>/dev/null || fail "an empty /dev/shm made it fail"
}

test_leaves_rings_when_the_wipe_did_not_happen() {
  reset_shm
  local ring="${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-44444444444444444444444444444444"
  : >"${ring}"
  # clear_path_preserving_mountpoint() discards the status of its rm -rf branch
  # and only warns for the mounted one, so a /data/cubelet that survived the
  # wipe reaches here unreported. The entry makes the next start attach and
  # replay this ring, so it must be kept.
  mkdir -p "${tmp}/state"
  : >"${tmp}/state/bstore.json"

  local out
  out="$(remove_s3lvol_writeback_rings 2>&1)"

  [[ -e "${ring}" ]] || fail "a ring was removed while its bstore.json entry survived"
  [[ "${out}" == *"bstore.json still exists"* ]] || fail "no warning about the surviving state: ${out}"

  rm -f "${tmp}/state/bstore.json"
  remove_s3lvol_writeback_rings 2>/dev/null
  [[ ! -e "${ring}" ]] || fail "the ring was not removed once bstore.json was gone"
}

test_leaves_rings_without_flock() {
  reset_shm
  local ring="${S3LVOL_SHM_DIR}/s3lvol-wb-0/s3lvol-wb-55555555555555555555555555555555"
  : >"${ring}"

  # Without the lock there is no way to tell a ring whose target is gone from
  # one a hand-started target still has open, so removing it would take the
  # acknowledged writes with it. A refusal, not a fallback.
  local out
  out="$(
    command() {
      if [[ "$*" == "-v flock" ]]; then return 1; fi
      builtin command "$@"
    }
    remove_s3lvol_writeback_rings 2>&1
  )"

  [[ -e "${ring}" ]] || fail "a ring was removed with no flock available"
  [[ "${out}" == *"flock is not available"* ]] || fail "no warning about the missing flock: ${out}"
}

test_wired_into_uninstall_and_check() {
  local remove_data check
  remove_data="$(sed -n '/^remove_data()/,/^}/p' "${UNINSTALL}")"
  check="$(sed -n '/^check_installation()/,/^}/p' "${UNINSTALL}")"
  [[ "${remove_data}" == *remove_s3lvol_writeback_rings* ]] \
    || fail "remove_data() does not remove the s3lvol write-back rings"
  [[ "${check}" == *'s3lvol-wb-*'* ]] \
    || fail "check_installation() does not report leftover write-back rings"
}

test_removes_both_layouts
test_leaves_rings_when_the_wipe_did_not_happen
test_leaves_rings_without_flock
test_leaves_a_held_ring
test_skips_symlinks
test_empty_shm_is_fine
test_wired_into_uninstall_and_check

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} failure(s)" >&2
  exit 1
fi
echo "test_uninstall_s3lvol_shm: all passed"
