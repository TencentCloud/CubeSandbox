#!/usr/bin/env bash
# Tests resolve_tagged_or_content_version.
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
  local got="$1"
  local want="$2"
  local msg="${3:-}"
  [[ "${got}" == "${want}" ]] || fail "${msg}: got='${got}' want='${want}'"
}

test_file_content_version_format() {
  local f="${TMP_DIR}/vmlinux-a"
  printf 'kernel-bytes-aaa\n' > "${f}"
  local ver
  ver="$(file_content_version "${f}")" || fail "file_content_version failed"
  [[ "${ver}" =~ ^sha256-[0-9a-f]{12}$ ]] || fail "unexpected format: ${ver}"
}

test_same_bytes_same_version() {
  local a="${TMP_DIR}/vmlinux-same-1"
  local b="${TMP_DIR}/vmlinux-same-2"
  printf 'identical-kernel\n' > "${a}"
  cp -f "${a}" "${b}"
  assert_eq "$(file_content_version "${a}")" "$(file_content_version "${b}")" "same content"
}

test_different_bytes_different_version() {
  local a="${TMP_DIR}/vmlinux-diff-1"
  local b="${TMP_DIR}/vmlinux-diff-2"
  printf 'kernel-one\n' > "${a}"
  printf 'kernel-two\n' > "${b}"
  local va vb
  va="$(file_content_version "${a}")"
  vb="$(file_content_version "${b}")"
  [[ "${va}" != "${vb}" ]] || fail "different content must differ: ${va}"
}

test_explicit_tag_wins() {
  local f="${TMP_DIR}/vmlinux-tagged"
  printf 'kernel-tagged\n' > "${f}"
  assert_eq "$(resolve_tagged_or_content_version "6.6.119-test" "${f}")" "6.6.119-test" "tag wins"
}

test_unknown_tag_falls_back_to_hash() {
  local f="${TMP_DIR}/vmlinux-unknown"
  printf 'kernel-unknown-tag\n' > "${f}"
  local hashed want
  hashed="$(file_content_version "${f}")"
  want="$(resolve_tagged_or_content_version "unknown" "${f}")"
  assert_eq "${want}" "${hashed}" "unknown tag"
  want="$(resolve_tagged_or_content_version "" "${f}")"
  assert_eq "${want}" "${hashed}" "empty tag"
}

test_file_content_version_format
test_same_bytes_same_version
test_different_bytes_different_version
test_explicit_tag_wins
test_unknown_tag_falls_back_to_hash

echo "kernel content version tests OK"
