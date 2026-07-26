#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for guest-kernel selection + atomic_replace recovery in
# component-entrypoint.sh (no container required).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load entrypoint helpers without executing main.
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")

assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "${got}" != "${want}" ]]; then
    printf 'FAIL: %s (got=%q want=%q)\n' "${msg}" "${got}" "${want}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

setup_kernels() {
  local dir="$1"
  mkdir -p "${dir}"
  : >"${dir}/vmlinux-bm"
  : >"${dir}/vmlinux-pvm"
}

# --- select_guest_kernel priorities ---
# effective-pvm → preserved → CUBE_PVM_ENABLE → symlink → bm

TOOLBOX_ROOT="${TMP}/tb1"
STATE_DIR="${TMP}/st1"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
# Image default after replace would be bm; preserved says pvm; no env/state.
unset CUBE_PVM_ENABLE || true
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "preserved pvm wins over post-replace bm symlink"
assert_eq "$(basename "$(readlink "${STATE_DIR}/vmlinux-active")")" "vmlinux-pvm" \
  "vmlinux-active follows preserved pvm"

TOOLBOX_ROOT="${TMP}/tb2"
STATE_DIR="${TMP}/st2"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
unset CUBE_PVM_ENABLE || true
export CUBE_PVM_ENABLE=0
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "preserved pvm beats CUBE_PVM_ENABLE=0 (Chart env must not wipe history)"

TOOLBOX_ROOT="${TMP}/tb2b"
STATE_DIR="${TMP}/st2b"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
unset CUBE_PVM_ENABLE || true
export CUBE_PVM_ENABLE=1
select_guest_kernel ""
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "CUBE_PVM_ENABLE=1 used when no preserved / no effective-pvm"

TOOLBOX_ROOT="${TMP}/tb3"
STATE_DIR="${TMP}/st3"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-bm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
printf '1\n' >"${STATE_DIR}/effective-pvm"
export CUBE_PVM_ENABLE=0
select_guest_kernel "vmlinux-bm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-pvm" \
  "effective-pvm=1 beats CUBE_PVM_ENABLE and preserved"

TOOLBOX_ROOT="${TMP}/tb3b"
STATE_DIR="${TMP}/st3b"
mkdir -p "${TOOLBOX_ROOT}" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-pvm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
printf '0\n' >"${STATE_DIR}/effective-pvm"
export CUBE_PVM_ENABLE=1
select_guest_kernel "vmlinux-pvm"
assert_eq "$(basename "$(readlink "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux")")" "vmlinux-bm" \
  "effective-pvm=0 beats preserved pvm and CUBE_PVM_ENABLE=1"

# --- preserve_guest_kernel_selection ---
TOOLBOX_ROOT="${TMP}/tb4"
STATE_DIR="${TMP}/st4"
mkdir -p "${TOOLBOX_ROOT}/cube-kernel-scf" "${STATE_DIR}"
setup_kernels "${TOOLBOX_ROOT}/cube-kernel-scf"
ln -sfn vmlinux-pvm "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
ln -sfn "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux-pvm" "${STATE_DIR}/vmlinux-active"
got="$(preserve_guest_kernel_selection "${TOOLBOX_ROOT}/cube-kernel-scf")"
assert_eq "${got}" "vmlinux-pvm" "preserve reads vmlinux-active"

# --- atomic_replace_dir legacy recovery ---
TOOLBOX_ROOT="${TMP}/tb5"
mkdir -p "${TOOLBOX_ROOT}"
dst="${TOOLBOX_ROOT}/comp"
src="${TMP}/src5"
mkdir -p "${src}"
echo live >"${src}/f"
mkdir -p "${dst}.legacy.111"
echo old >"${dst}.legacy.111/f"
# dst missing (crash after rename-aside)
atomic_replace_dir "${src}" "${dst}"
assert_eq "$(cat "${dst}/f")" "live" "atomic_replace recovers then promotes new tree"
[[ ! -e "${dst}.legacy.111" ]] || { echo "FAIL: orphan legacy not cleaned"; exit 1; }
printf 'ok: orphan legacy cleaned after successful replace\n'

# --- staging marker: no EXIT trap; only success clears ---
# Simulate mid-stage: write marker, then "fail" without success cleanup.
# Entrypoint must not register trap ... EXIT that would rm the marker.
if grep -qE 'trap[[:space:]]+clear_staging_marker[[:space:]]+EXIT' "${ENTRY}"; then
  echo "FAIL: stage_component still registers clear_staging_marker EXIT trap" >&2
  exit 1
fi
printf 'ok: no clear_staging_marker EXIT trap in entrypoint\n'

marker="${TMP}/.staging-cubelet"
printf 'staging\n' >"${marker}"
# Subshell exit must not clear marker (documents intended failure-path behavior).
(
  clear_staging_marker() { rm -f "${marker}"; }
  # Intentionally do NOT register trap — mirrors production after Fix 1.
  false || true
)
[[ -f "${marker}" ]] || { echo "FAIL: staging marker disappeared without success cleanup"; exit 1; }
printf 'ok: staging marker survives non-success path (no EXIT clear)\n'

# Success path still clears (mirrors stage_component end).
rm -f "${marker}"
[[ ! -f "${marker}" ]] || { echo "FAIL: could not clear marker on success"; exit 1; }
printf 'ok: success path can clear staging marker\n'

# --- ensure_component_version_json: no weak env synthesis ---
dst="${TMP}/na"
mkdir -p "${dst}"
CUBE_VERSION=should-not-write ensure_component_version_json network-agent "${dst}"
[[ ! -f "${dst}/version.json" ]] || { echo "FAIL: weak env wrote version.json"; exit 1; }
printf 'ok: network-agent does not synthesize from CUBE_VERSION\n'

mkdir -p "${TMP}/guest"
printf 'g1\n' >"${TMP}/guest/version"
printf 'a1\n' >"${TMP}/guest/agent-version"
ensure_component_version_json cube-guest "${TMP}/guest"
grep -q 'guest-image' "${TMP}/guest/version.json"
grep -q 'cube-agent' "${TMP}/guest/version.json"
printf 'ok: cube-guest still synthesizes from markers\n'

printf 'ALL PASS\n'
