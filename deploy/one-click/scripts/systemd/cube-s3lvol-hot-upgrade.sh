#!/usr/bin/env bash
# Copyright (C) 2026 Tencent. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# cube-s3lvol-hot-upgrade.sh -- swap the s3lvol target under a live sandbox
#
# Called by install.sh --mode=upgrade, before anything else is stopped. That
# placement is the whole point: an online flush needs the target that is running
# *and* the S3 endpoint it writes to, and both are gone by the time the rest of
# the install has been through.
#
# The new component is already installed under its versioned directory, and the
# bare name still points at the outgoing build. This script decides whether the
# swap can be done without taking a live sandbox's block devices away, does it,
# and puts the bare name back if it cannot.
#
# What makes it "in place": the target is killed outright rather than stopped,
# so the host's nvme_tcp controllers go into error recovery with their gendisks
# intact, and the replacement rebuilds the same NQN/(subsys, nsid)/UUID grid.
# Nothing here disconnects the initiator or unloads the lvstore.
#
# Usage: cube-s3lvol-hot-upgrade.sh <new-version-directory-name> [old-version-directory]
#
# The old directory is passed in by install.sh, which has to capture it before
# it stages the new one: once the bare name has been switched there is nothing
# left to resolve it through. Without the argument -- a hand invocation -- it is
# read off the bare name here.
#
# Exit: 0  the target is on the new build (upgraded, or there was nothing to
#          upgrade because none was running)
#       non-zero  it is on the old one, or nothing is running. The caller must
#          not read this as "the install failed" -- the rest of the install is
#          independent -- but it must not report the upgrade as done either.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOLBOX_ROOT="${TOOLBOX_ROOT:-$(cd "${SELF_DIR}/../.." && pwd)}"
INSTALL_PREFIX="${TOOLBOX_ROOT}"
BARE="${INSTALL_PREFIX}/CubeS3lvol"
SERVICE="cube-sandbox-s3lvol.service"
SNAPSHOT="/var/tmp/rcow/hot-upgrade.snapshot"

log() { echo "[one-click] CubeS3lvol: $*"; }
warn() { echo "[one-click] CubeS3lvol: WARNING: $*" >&2; }

NEW_VERSION="${1:-}"
if [ -z "${NEW_VERSION}" ]; then
  warn "usage: $0 <new-version-directory-name> [old-version-directory]"
  exit 2
fi
NEW_DIR="${INSTALL_PREFIX}/${NEW_VERSION}"
if [ ! -x "${NEW_DIR}/scripts/rcow_common.sh" ]; then
  warn "'${NEW_DIR}' does not look like a CubeS3lvol install"
  exit 2
fi

# The outgoing build. Passed in by install.sh, or resolved off the bare name
# while it still points at it.
OLD_DIR="${2:-}"
if [ -z "${OLD_DIR}" ]; then
  OLD_DIR="$(readlink -f "${BARE}" 2>/dev/null || true)"
fi

switch_bare_to() {
  # Through a rename, so the bare name never points at nothing.
  ln -sfn "$1" "${INSTALL_PREFIX}/.CubeS3lvol.new"
  mv -Tf "${INSTALL_PREFIX}/.CubeS3lvol.new" "${BARE}"
}

# shellcheck source=/dev/null
. "${NEW_DIR}/scripts/rcow_common.sh"

# The scripts above resolve RCOW_TGT_BIN through the NEW directory, but the
# process that is running is from the outgoing one -- and the identity check
# behind the marker write compares the two paths. Point it at what is actually
# running; the version gate takes its candidate as an argument and is not
# affected.
if [ -n "${OLD_DIR}" ] && [ -x "${OLD_DIR}/bin/s3lvol_tgt" ]; then
  RCOW_TGT_BIN="${OLD_DIR}/bin/s3lvol_tgt"
  export RCOW_TGT_BIN
fi

# Stop the service and confirm the target is gone.
#
# `reset-failed` first, because a stop on a failed unit is a no-op: a previous
# refusal or crash-loop leaves the unit in failed while the target keeps
# running, and nothing downstream of a no-op stop is doing what it thinks.
#
# The SIGKILL fallback is for the case where the stop script cannot identify
# the target -- the bare name has been switched, so the strict path comparison
# behind rcow_target_alive fails, and the stop refuses rather than signal a
# process it cannot name. In the context of an upgrade that is over-cautious:
# the WAL makes every acknowledged write durable, which is the same guarantee
# the hot path's own SIGKILL rests on, and whatever is running is about to be
# replaced anyway.
stop_and_confirm() {
  systemctl reset-failed "${SERVICE}" >/dev/null 2>&1 || true
  systemctl stop "${SERVICE}" >/dev/null 2>&1 || true
  local i pid
  for i in $(seq 1 15); do
    [ -z "$(rcow_target_instances)" ] && return 0
    sleep 1
  done
  for pid in $(rcow_target_instances); do
    warn "the stop could not reach target pid ${pid}; killing it directly"
    kill -9 "${pid}" 2>/dev/null || true
  done
  for i in $(seq 1 10); do
    [ -z "$(rcow_target_instances)" ] && return 0
    sleep 1
  done
  warn "the target is still running; it is holding the WAL"
  return 1
}

# Start the service and wait for the target to be up.
#
# `systemctl start` on a Type=simple unit returns when the supervise process is
# running, not when the target it supervises is up -- and a supervise whose
# rcow_start.sh failed exits and restarts, so "start returned 0" says nothing
# about the outcome. What says something is a target being there.
start_and_wait() {
  systemctl reset-failed "${SERVICE}" >/dev/null 2>&1 || true
  systemctl start "${SERVICE}" >/dev/null 2>&1 || return 1
  local i
  for i in $(seq 1 60); do
    if [ -n "$(rcow_target_instances)" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# Nothing running is not a failure: the install's own start brings the new build
# up, and that is the upgrade.
if [ -z "$(rcow_target_instances)" ]; then
  log "no target is running; the new build comes up with the rest of the install"
  exit 0
fi

# ==========================================================================
# Can this be done in place?
#
# Two things have to hold, and they are independent:
#
#   - the running build has to be able to describe itself, because nothing can
#     learn its on-disk formats after it is killed. A target from before that
#     RPC exists cannot, and rcow_version_gate_check says so;
#   - the running build's scripts have to contain the one the stop path calls.
#     A build from before that script was renamed has the old name only, and the
#     stop would refuse rather than tear the namespaces down -- correctly, but it
#     means this is a cold upgrade.
#
# Either one failing is a cold upgrade, which is what the install would have
# done anyway; the difference is that it is now said out loud.
MODE=hot
REASON=""

if ! rcow_version_gate_check "${NEW_DIR}/bin/s3lvol_tgt"; then
  MODE=cold
  REASON="the version gate refused it (above)"
fi
if [ -n "${OLD_DIR}" ] && [ ! -x "${OLD_DIR}/scripts/rcow_upgrade.sh" ]; then
  MODE=cold
  REASON="the running build has no scripts/rcow_upgrade.sh, so it cannot flush online"
fi

# ==========================================================================
if [ "${MODE}" = "hot" ]; then
  log "upgrading in place: a live sandbox's I/O pauses for the swap, nothing else"

  # The candidate travels in the marker: the stop runs before the switch, so at
  # that point RCOW_TGT_BIN still names the outgoing binary and a gate handed it
  # would compare the running build with itself.
  if ! rcow_hot_marker_write "${NEW_DIR}/bin/s3lvol_tgt"; then
    warn "could not record the intent to upgrade; falling back to a cold stop"
    MODE=cold
    REASON="the hot-restart marker could not be written"
  fi
fi

if [ "${MODE}" = "hot" ]; then
  if ! systemctl stop "${SERVICE}"; then
    warn "the in-place stop failed; falling back to a cold stop"
    MODE=cold
    REASON="the stop did not succeed"
  elif [ -n "$(rcow_target_instances)" ]; then
    # Should not happen: the stop's own kill is verified. If it does, the target
    # is still holding the WAL and a second one must not be started over it.
    warn "the target is still running after the stop; leaving everything alone"
    exit 1
  fi
fi

if [ "${MODE}" = "cold" ]; then
  # Loudly, because this one is an outage: the initiator is disconnected and the
  # lvstore unloaded, so every live sandbox loses its block devices.
  warn "upgrading with a cold stop, which WILL interrupt a live sandbox's I/O"
  [ -n "${REASON}" ] && warn "  because ${REASON}"
  warn "  a target that predates this mechanism can only be upgraded this way once;"
  warn "  the next upgrade finds a build that can, and goes in place"
  stop_and_confirm || exit 1
fi

# ==========================================================================
# The swap, and the way back.
switch_bare_to "${NEW_VERSION}"
log "the bare name now points at ${NEW_VERSION}"

# The hot path leaves a snapshot from its own online step, so the layout can be
# compared position by position. A cold stop has none -- its stop is the planned
# one, which restores the grid from bstore.json and the registry -- so there the
# check is the weaker "every recorded volume resolves".
verify() {
  if [ "${MODE}" = "hot" ] && [ -s "${SNAPSHOT}" ]; then
    rcow_verify_active --expect "${SNAPSHOT}" 60
  else
    rcow_verify_active 60
  fi
}

if start_and_wait && verify >/dev/null 2>&1; then
  log "upgraded; the layout is the one it had before"
  exit 0
fi

# Roll back. The replacement never got as far as writing anything (an attach
# that fails writes nothing), so the outgoing build can take the same WAL back.
warn "the new build did not come up with the layout intact; rolling back to ${OLD_DIR}"
stop_and_confirm || true
if [ -n "${OLD_DIR}" ]; then
  switch_bare_to "$(basename "${OLD_DIR}")"
  if start_and_wait; then
    warn "rolled back to the previous build; it is running and this upgrade did not happen"
  else
    warn "the previous build did not come back either: recover by hand, see the runbook"
  fi
fi
exit 1
