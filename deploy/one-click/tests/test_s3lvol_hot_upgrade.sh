#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the CubeS3lvol upgrade orchestrator's invocation contract.
#
# install.sh runs cube-s3lvol-hot-upgrade.sh **out of the unpacked package
# tree**, because the copy in the install tree is the one being replaced (an
# install from before this has an older script there, or none). That makes the
# install prefix something the script cannot derive from its own location, and
# something it must not silently get wrong: derived from the package tree it
# points at the temp directory the package was extracted into, the version
# directory is not found, and the script exits 2 having done nothing -- which
# install.sh reports as the whole upgrade failing.
#
# No other caller catches that: every other invocation is by hand, against a
# real install tree, where the derivation happens to be right. So the invocation
# shape is exercised here, on the tree layout install.sh actually produces.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

INSTALL_SH="${ONE_CLICK_DIR}/install.sh"
HOT_UPGRADE="${ONE_CLICK_DIR}/scripts/systemd/cube-s3lvol-hot-upgrade.sh"
RCOW_COMMON="${REPO_ROOT}/CubeS3lvol/scripts/rcow_common.sh"
NEW_VERSION="CubeS3lvol-2.0"
OLD_VERSION="CubeS3lvol-1.0"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"
}

assert_contains_text() {
  [[ "$1" == *"$2"* ]] || fail "expected output to contain: $2 (got: $1)"
}

# A version directory holding the scripts the orchestrator sources from it.
# rcow_common.sh sources its own cpu-mask helper by path, so a tree without that
# file reports a missing file at source time.
make_version_dir() {
  local dir="${TMP_DIR}/prefix/$1"
  mkdir -p "${dir}/scripts"
  cp "${RCOW_COMMON}" "${dir}/scripts/"
  cp "${REPO_ROOT}/CubeS3lvol/scripts/rcow_cpumask.sh" "${dir}/scripts/"
}

# The two trees install.sh works with: the package it runs the script from, and
# the install prefix holding the versioned component.
build_trees() {
  rm -rf "${TMP_DIR}/pkg" "${TMP_DIR}/prefix" "${TMP_DIR}/run"
  mkdir -p "${TMP_DIR}/pkg/scripts/systemd" "${TMP_DIR}/run"
  cp "${HOT_UPGRADE}" "${TMP_DIR}/pkg/scripts/systemd/"

  make_version_dir "${OLD_VERSION}"
  make_version_dir "${NEW_VERSION}"
  # The bare name, as install.sh leaves it while the outgoing build is still
  # running: the new version staged beside it, not switched to it.
  ln -sfn "${OLD_VERSION}" "${TMP_DIR}/prefix/CubeS3lvol"
}

bare_points_at() {
  basename "$(readlink -f "${TMP_DIR}/prefix/CubeS3lvol" 2>/dev/null || true)"
}

# Run the orchestrator from the package tree.
#
# Every path it could match a running target through is pointed at this tmpdir,
# so the answer does not depend on whether the machine running the tests happens
# to have a target of its own. RCOW_LVS_NAME is set because sourcing
# rcow_common.sh resolves an lvstore name when it has none, and exits if it
# cannot -- a named lvstore is how every dataplane suite isolates itself too.
#
# An empty first argument reproduces the invocation shape that omitted the
# prefix; anything else is passed as TOOLBOX_ROOT.
run_orchestrator() {
  local root="$1"
  local -a envargs=(
    RCOW_LVS_NAME=hotupgradetest
    RCOW_RUN_DIR="${TMP_DIR}/run"
    RCOW_TGT_BIN="${TMP_DIR}/gone/s3lvol_tgt"
    RCOW_RPC_SOCK="${TMP_DIR}/run/no.sock"
  )
  if [[ -n "${root}" ]]; then
    envargs+=(TOOLBOX_ROOT="${root}")
  fi
  env "${envargs[@]}" \
    bash "${TMP_DIR}/pkg/scripts/systemd/cube-s3lvol-hot-upgrade.sh" "${NEW_VERSION}"
}

test_the_prefix_travels_with_the_invocation() {
  build_trees
  local out="" rc=0
  out="$(run_orchestrator "${TMP_DIR}/prefix" 2>&1)" || rc=$?
  [[ "${rc}" -eq 0 ]] ||
    fail "the orchestrator should run against the prefix it is given (rc=${rc}): ${out}"
  # Reached the version directory, then found nothing to upgrade -- which is the
  # install's own start doing it.
  assert_contains_text "${out}" "no target is running"
  # Nothing was running, so the upgrade is the bare name moving to the staged
  # build. That half is the orchestrator's now: install.sh stages without
  # switching, because the running build is recognised through the bare name.
  [[ "$(bare_points_at)" == "${NEW_VERSION}" ]] ||
    fail "the orchestrator must switch the bare name when no target is running (got $(bare_points_at))"
}

test_without_the_prefix_it_refuses() {
  build_trees
  local out="" rc=0
  out="$(run_orchestrator "" 2>&1)" || rc=$?
  # The negative control. Deriving the prefix from the copy under test finds the
  # package root instead of the install, and reports the upgrade as failed.
  [[ "${rc}" -eq 2 ]] ||
    fail "expected exit 2 when the prefix is derived from the package tree (got ${rc}): ${out}"
  assert_contains_text "${out}" "does not look like a CubeS3lvol install"
  # It refused before doing anything, so the bare name is where it was.
  [[ "$(bare_points_at)" == "${OLD_VERSION}" ]] ||
    fail "a run that refuses must leave the bare name on the outgoing build (got $(bare_points_at))"
}

test_staging_leaves_the_bare_name_alone() {
  # The running build is recognised through the bare name: its supervisor polls
  # rcow_target_alive, and the unit's stop script reaches its scripts through it.
  # Switching the bare name at staging time therefore takes the live build's own
  # supervisor down mid-swap -- and the stop that follows becomes a no-op on the
  # failed unit, so the orchestrator's guard finds the target it was asked to
  # replace still running and refuses the upgrade.
  if awk '/^install_cubes3lvol_versioned\(\)/,/^}/' "${INSTALL_SH}" |
    grep -qE 'mv -Tf .*CubeS3lvol'; then
    fail "install_cubes3lvol_versioned must stage without switching the bare name"
  fi

  # The switch has to happen somewhere. The orchestrator does it, and the
  # install does it only when the orchestrator is not going to run.
  assert_contains "${INSTALL_SH}" '"${S3LVOL_STAGED_DIR}" "${S3LVOL_OLD_DIR}"'
  assert_contains "${INSTALL_SH}" 'switch_cubes3lvol_bare_to "${S3LVOL_STAGED_DIR}"'
  assert_contains "${HOT_UPGRADE}" 'switch_bare_to "${NEW_VERSION}"'
}

test_the_hot_stop_clears_a_failed_unit() {
  # A stop on a failed unit is a no-op that reports success, and a unit left
  # failed by an earlier refusal or crash-loop is a state an upgrade can arrive
  # in. The cold path resets that state; the hot path has to as well, or the
  # stop does nothing while the guard below it refuses.
  awk '/rm -f "\$\{RCOW_HOT_SNAPSHOT\}"/,/systemctl stop "\$\{SERVICE\}"/' "${HOT_UPGRADE}" |
    grep -qF 'systemctl reset-failed' ||
    fail "the hot stop must reset a failed unit before stopping it"
}

test_install_sh_hands_over_the_prefix() {
  # Structural, because install.sh needs root, KVM and a real node to run. The
  # prefix has to be on the invocation itself rather than exported somewhere
  # earlier: this is the only script install.sh runs out of the package tree.
  local block prev
  block="$(awk '
    /^[[:space:]]*(bash )?"\$\{PKG_ROOT\}\/scripts\/systemd\/cube-s3lvol-hot-upgrade\.sh"/ {
      print prev; print; found = 1
    }
    { prev = $0 }
    END { exit(found ? 0 : 1) }
  ' "${INSTALL_SH}")" || fail "install.sh no longer invokes cube-s3lvol-hot-upgrade.sh"

  prev="$(printf '%s\n' "${block}" | head -1)"
  assert_contains_text "${prev}" 'TOOLBOX_ROOT="${INSTALL_PREFIX}"'
  assert_contains_text "${block}" 'bash "${PKG_ROOT}/scripts/systemd/cube-s3lvol-hot-upgrade.sh"'
}

test_layout_check_takes_the_snapshot_from_rcow_common() {
  # A restated default silently downgrades the strong check to the weak one
  # wherever RCOW_RUN_DIR is overridden, and still reports it as passed.
  assert_contains "${HOT_UPGRADE}" 'rcow_verify_active --expect "${RCOW_HOT_SNAPSHOT}"'
  if grep -Fq '/var/tmp/rcow/hot-upgrade.snapshot' "${HOT_UPGRADE}"; then
    fail "the orchestrator must take the snapshot path from rcow_common.sh, not restate its default"
  fi
}

test_the_layout_guard_reads_what_it_sources() {
  # -x on a file that is only ever sourced reports a healthy tree as broken.
  if grep -Fq '[ ! -x "${NEW_DIR}/scripts/rcow_common.sh" ]' "${HOT_UPGRADE}"; then
    fail "the version-directory guard must test rcow_common.sh with -f: it is sourced, not executed"
  fi
  assert_contains "${HOT_UPGRADE}" '[ ! -f "${NEW_DIR}/scripts/rcow_common.sh" ]'
}

# Run the readiness predicate out of the real rcow_common.sh with its two probes
# stubbed: a target process is there in every case, and only the answer to
# rcow_get_lvstores changes. Prints "ready" or "not-ready".
probe_ready() {
  local stores="$1" rc_stub="${2:-0}"
  RCOW_LVS_NAME=rcow-test \
  RCOW_RUN_DIR="${TMP_DIR}/run" \
  RCOW_ACTIVE_FILE="${TMP_DIR}/run/active_lvols" \
  RCOW_BSTORE_FILE="${TMP_DIR}/run/bstore.json" \
  RCOW_RPC_SOCK="${TMP_DIR}/run/no.sock" \
  RCOW_TGT_BIN="${TMP_DIR}/gone/s3lvol_tgt" \
  STORES="${stores}" RC_STUB="${rc_stub}" \
    bash -c '
      set -u
      . "$1"
      rcow_target_instances() { printf "%s\n" 4242; }
      rcow_rpc() { printf "%s" "${STORES}"; return "${RC_STUB}"; }
      rcow_target_ready && echo ready || echo not-ready
    ' _ "${TMP_DIR}/prefix/${NEW_VERSION}/scripts/rcow_common.sh"
}

test_readiness_is_the_attach_not_the_process() {
  # The distinction this whole wait exists for. A replacement answers RPCs --
  # including rcow_get_bdev, which is what the layout check reads -- before it
  # has attached anything, because the device paths in that answer come from the
  # registry and the host's sysfs, which the kill left in place. Readiness has
  # to be the attach, or the upgrade declares success and then stops the S3
  # endpoint the attach still needs.
  build_trees

  [[ "$(probe_ready '[{"lvs_name":"rcow-test"}]')" == ready ]] ||
    fail "a target holding this host's lvstore must read as ready"
  [[ "$(probe_ready '[]')" == not-ready ]] ||
    fail "a running target with no lvstore loaded must not read as ready"
  [[ "$(probe_ready '[{"lvs_name":"rcow-other"}]')" == not-ready ]] ||
    fail "another lvstore's entry must not read as this host's"
  [[ "$(probe_ready 'not json')" == not-ready ]] ||
    fail "an unparseable answer must not read as ready"
  [[ "$(probe_ready '' 1)" == not-ready ]] ||
    fail "an RPC that fails must not read as ready"
}

test_the_start_wait_gates_on_readiness() {
  # Structural: the behavioural half needs a machine to start a target on.
  awk '/^start_and_wait\(\)/,/^}/' "${HOT_UPGRADE}" | grep -qF 'rcow_target_ready' ||
    fail "start_and_wait must wait for rcow_target_ready, not for a target process to exist"
}

test_the_install_root_is_checked_before_anything_is_touched() {
  # A refused install root used to cost a half-upgraded node: the check ran after
  # the swap and after the services were stopped, so the refusal left s3lvol
  # upgraded, everything else stopped, and no way forward but a re-run.
  local assert_line stop_line stage_line
  assert_line="$(grep -nE '^assert_safe_install_prefix "\$\{INSTALL_PREFIX\}"' "${INSTALL_SH}" |
    head -1 | cut -d: -f1 || true)"
  stop_line="$(grep -nE '^stop_existing_systemd_deployment$' "${INSTALL_SH}" |
    head -1 | cut -d: -f1 || true)"
  stage_line="$(grep -nE '^[[:space:]]*install_cubes3lvol_versioned "\$\{PKG_ROOT\}/CubeS3lvol"' "${INSTALL_SH}" |
    head -1 | cut -d: -f1 || true)"

  [[ -n "${assert_line}" ]] || fail "install.sh no longer asserts the install prefix is safe"
  [[ -n "${stop_line}" ]] || fail "install.sh no longer stops the deployment"
  [[ -n "${stage_line}" ]] || fail "install.sh no longer stages the versioned CubeS3lvol"
  [[ "${assert_line}" -lt "${stage_line}" && "${assert_line}" -lt "${stop_line}" ]] ||
    fail "install.sh must check the install prefix first (assert=${assert_line}, stage=${stage_line}, stop=${stop_line})"
}

test_the_prefix_travels_with_the_invocation
test_without_the_prefix_it_refuses
test_install_sh_hands_over_the_prefix
test_staging_leaves_the_bare_name_alone
test_the_hot_stop_clears_a_failed_unit
test_layout_check_takes_the_snapshot_from_rcow_common
test_the_layout_guard_reads_what_it_sources
test_readiness_is_the_attach_not_the_process
test_the_start_wait_gates_on_readiness
test_the_install_root_is_checked_before_anything_is_touched

echo "CubeS3lvol hot upgrade tests OK"
