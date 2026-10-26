#!/usr/bin/env bash
# Tests inventory_kernel_content_variants from install.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_eq() {
  local got="$1" want="$2" msg="${3:-}"
  [[ "${got}" == "${want}" ]] || fail "${msg}: got='${got}' want='${want}'"
}

COMPONENT_VERSIONS_ROOT="${TMP_DIR}/inv"

# Source inventory_kernel_content_variants from install.sh without running main.
# shellcheck disable=SC1090
source <(awk '
  /^inventory_kernel_content_variants\(\)/ {p=1}
  p {print}
  p && /^}/ {exit}
' "${ONE_CLICK_DIR}/install.sh")

declare -f inventory_kernel_content_variants >/dev/null \
  || fail "failed to load inventory_kernel_content_variants from install.sh"

test_dual_variant_content_inventory() {
  local src="${TMP_DIR}/pkg/cube-kernel-scf"
  mkdir -p "${src}"
  printf 'bm-kernel-bytes\n' > "${src}/vmlinux-bm"
  printf 'pvm-kernel-bytes\n' > "${src}/vmlinux-pvm"

  local bm_digest pvm_digest bm_short pvm_short
  bm_digest="$(file_sha256_hex "${src}/vmlinux-bm")"
  pvm_digest="$(file_sha256_hex "${src}/vmlinux-pvm")"
  bm_short="sha256-${bm_digest:0:12}"
  pvm_short="sha256-${pvm_digest:0:12}"

  inventory_kernel_content_variants "${src}"

  [[ -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}" ]] || fail "missing bm dir"
  [[ -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}" ]] || fail "missing pvm dir"
  assert_eq "$(cat "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}/variant")" "bm"
  assert_eq "$(cat "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/variant")" "pvm"
  assert_eq "$(readlink "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}/vmlinux")" "vmlinux-bm"
  assert_eq "$(readlink "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/vmlinux")" "vmlinux-pvm"
  assert_eq "$(tr -d '[:space:]' < "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${bm_short}/version")" "sha256:${bm_digest}"
  assert_eq "$(tr -d '[:space:]' < "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/${pvm_short}/version")" "sha256:${pvm_digest}"

  if ls -d "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf/"*@* >/dev/null 2>&1; then
    fail "must not create tag@digest directories"
  fi

  inventory_kernel_content_variants "${src}"
  local count
  count="$(find "${COMPONENT_VERSIONS_ROOT}/cube-kernel-scf" -mindepth 1 -maxdepth 1 -type d | wc -l)"
  [[ "${count}" -eq 2 ]] || fail "expected exactly 2 inventory dirs, got ${count}"
}

test_dual_variant_content_inventory
echo "ALL PASS"
