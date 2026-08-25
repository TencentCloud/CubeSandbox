#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cloud-free checks for the AWS CLI pack cache and install-deps source
# selection. Does not install AWS CLI or touch awscli.amazonaws.com.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
INSTALL_DEPS="${ROOT_DIR}/examples/volume/s3/install-deps.sh"
PIN_VERSION_FILE="${ONE_CLICK_DIR}/assets/vendor/awscli/VERSION"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"
# shellcheck source=../lib/awscli-bundle.sh
source "${ONE_CLICK_DIR}/lib/awscli-bundle.sh"

PIN_DIR=""
failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="$3"
  [[ "${got}" == "${want}" ]] || fail "${msg}: got '${got}' want '${want}'"
}

write_fail_curl() {
  local path="$1"
  local marker="$2"
  cat > "${path}" <<SH
#!/usr/bin/env bash
printf 'invoked %s\\n' "\$*" >> "${marker}"
echo "AWSCLI_FETCH_CURL must not download" >&2
exit 1
SH
  chmod +x "${path}"
}

write_success_curl() {
  local path="$1"
  local marker="$2"
  local payload="$3"
  cat > "${path}" <<SH
#!/usr/bin/env bash
printf 'invoked %s\\n' "\$*" >> "${marker}"
out=""
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    -o) out="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -n "\${out}" ]] || exit 1
cp -f "${payload}" "\${out}"
SH
  chmod +x "${path}"
}

# Sets PIN_DIR plus ONE_CLICK_AWSCLI_* in the current shell (do not capture
# this function in $() — that would drop the exports).
setup_pin_env() {
  local name="$1"
  local body="$2"
  local digest filename
  PIN_DIR="${TMP_DIR}/${name}"
  mkdir -p "${PIN_DIR}/cache"
  printf '%s' "${body}" > "${PIN_DIR}/payload.zip"
  digest="$(file_sha256_hex "${PIN_DIR}/payload.zip")"
  filename="awscli-exe-linux-x86_64-9.9.9.zip"
  printf '%s  %s\n' "${digest}" "${filename}" > "${PIN_DIR}/SHA256SUMS"
  export ONE_CLICK_AWSCLI_VERSION="9.9.9"
  export ONE_CLICK_AWSCLI_ARCH="x86_64"
  export ONE_CLICK_AWSCLI_CACHE_DIR="${PIN_DIR}/cache"
  export ONE_CLICK_AWSCLI_SHA256SUMS="${PIN_DIR}/SHA256SUMS"
  unset ONE_CLICK_AWSCLI_ZIP
}

test_version_pins_match() {
  local pinned
  pinned="$(tr -d '[:space:]' < "${PIN_VERSION_FILE}")"
  [[ -n "${pinned}" ]] || fail "VERSION pin is empty"
  grep -q -F "AWSCLI_VERSION=\"\${AWSCLI_VERSION:-${pinned}}\"" "${INSTALL_DEPS}" \
    || fail "install-deps.sh AWSCLI_VERSION default must match ${PIN_VERSION_FILE}"
}

test_cache_hit_skips_download() {
  local marker got
  setup_pin_env hit 'cached-awscli'
  marker="${PIN_DIR}/curl.marker"
  cp -f "${PIN_DIR}/payload.zip" "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip"
  write_fail_curl "${PIN_DIR}/fail-curl" "${marker}"
  export AWSCLI_FETCH_CURL="${PIN_DIR}/fail-curl"
  got="$(resolve_awscli_bundle_zip)"
  assert_eq "${got}" "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip" "cache hit path"
  [[ ! -e "${marker}" ]] || fail "cache hit must not invoke curl"
}

test_override_skips_download() {
  local marker got
  setup_pin_env override 'override-awscli'
  marker="${PIN_DIR}/curl.marker"
  cp -f "${PIN_DIR}/payload.zip" "${PIN_DIR}/override.zip"
  write_fail_curl "${PIN_DIR}/fail-curl" "${marker}"
  export AWSCLI_FETCH_CURL="${PIN_DIR}/fail-curl"
  export ONE_CLICK_AWSCLI_ZIP="${PIN_DIR}/override.zip"
  got="$(resolve_awscli_bundle_zip)"
  assert_eq "${got}" "${PIN_DIR}/override.zip" "override path"
  [[ ! -e "${marker}" ]] || fail "ONE_CLICK_AWSCLI_ZIP must not invoke curl"
}

test_cache_miss_downloads_once() {
  local marker got
  setup_pin_env miss 'downloaded-awscli'
  marker="${PIN_DIR}/curl.marker"
  write_success_curl "${PIN_DIR}/ok-curl" "${marker}" "${PIN_DIR}/payload.zip"
  export AWSCLI_FETCH_CURL="${PIN_DIR}/ok-curl"
  got="$(resolve_awscli_bundle_zip)"
  assert_eq "${got}" "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip" "download cache path"
  [[ -f "${marker}" ]] || fail "cache miss must invoke curl"
  [[ -f "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip" ]] \
    || fail "download must write the cache zip"
}

test_bad_cache_redownloads() {
  local marker
  setup_pin_env bad 'good-awscli'
  marker="${PIN_DIR}/curl.marker"
  printf 'corrupt' > "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip"
  write_success_curl "${PIN_DIR}/ok-curl" "${marker}" "${PIN_DIR}/payload.zip"
  export AWSCLI_FETCH_CURL="${PIN_DIR}/ok-curl"
  resolve_awscli_bundle_zip >/dev/null
  [[ -f "${marker}" ]] || fail "sha256 mismatch must re-download"
  assert_eq "$(cat "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip")" "good-awscli" \
    "re-download must replace corrupt cache"
}

test_install_deps_prefers_bundle_zip() {
  local out
  printf 'local-bundle' > "${TMP_DIR}/bundle.zip"
  out="$(AWSCLI_BUNDLE_ZIP="${TMP_DIR}/bundle.zip" "${INSTALL_DEPS}" --print-aws-source)"
  assert_eq "${out}" $'bundle\t'"${TMP_DIR}/bundle.zip" "AWSCLI_BUNDLE_ZIP source"
}

test_install_deps_prefers_packaged_vendor() {
  local root arch out
  arch="$(uname -m | sed -e 's/amd64/x86_64/' -e 's/arm64/aarch64/')"
  case "${arch}" in
    x86_64|aarch64) ;;
    *) fail "unsupported test host arch ${arch}"; return ;;
  esac
  root="${TMP_DIR}/sandbox-package"
  mkdir -p "${root}/CubeMaster/plugin" "${root}/support/vendor/awscli"
  cp -f "${INSTALL_DEPS}" "${root}/CubeMaster/plugin/install-s3-deps.sh"
  chmod +x "${root}/CubeMaster/plugin/install-s3-deps.sh"
  printf 'packaged' > "${root}/support/vendor/awscli/awscli-exe-linux-${arch}.zip"
  out="$("${root}/CubeMaster/plugin/install-s3-deps.sh" --print-aws-source)"
  assert_eq "${out}" $'bundle\t'"${root}/support/vendor/awscli/awscli-exe-linux-${arch}.zip" \
    "packaged vendor zip source"
}

test_install_deps_fallback_is_versioned() {
  local pinned out kind url
  pinned="$(tr -d '[:space:]' < "${PIN_VERSION_FILE}")"
  out="$(AWSCLI_VERSION="${pinned}" "${INSTALL_DEPS}" --print-aws-source)"
  kind="${out%%$'\t'*}"
  url="${out#*$'\t'}"
  assert_eq "${kind}" "download" "standalone source kind"
  [[ "${url}" == *"-${pinned}.zip" ]] || fail "fallback URL must pin ${pinned}, got ${url}"
  [[ "${url}" != *awscli-exe-linux-x86_64.zip ]] \
    || fail "fallback URL must not be the unversioned latest zip"
}

test_stage_copies_arch_named_zip() {
  local pkg dest
  setup_pin_env stage 'staged-awscli'
  cp -f "${PIN_DIR}/payload.zip" "${PIN_DIR}/cache/awscli-exe-linux-x86_64-9.9.9.zip"
  write_fail_curl "${PIN_DIR}/fail-curl" "${PIN_DIR}/curl.marker"
  export AWSCLI_FETCH_CURL="${PIN_DIR}/fail-curl"
  pkg="${PIN_DIR}/package"
  mkdir -p "${pkg}/support"
  stage_awscli_bundle_into_package "${pkg}"
  dest="${pkg}/support/vendor/awscli/awscli-exe-linux-x86_64.zip"
  [[ -f "${dest}" ]] || fail "staged zip missing: ${dest}"
  assert_eq "$(cat "${dest}")" "staged-awscli" "staged zip contents"
  assert_eq "$(tr -d '[:space:]' < "${pkg}/support/vendor/awscli/VERSION")" "9.9.9" \
    "staged VERSION"
  [[ ! -e "${PIN_DIR}/curl.marker" ]] || fail "stage from cache must not invoke curl"
}

test_version_pins_match
test_cache_hit_skips_download
test_override_skips_download
test_cache_miss_downloads_once
test_bad_cache_redownloads
test_install_deps_prefers_bundle_zip
test_install_deps_prefers_packaged_vendor
test_install_deps_fallback_is_versioned
test_stage_copies_arch_named_zip

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} awscli-bundle test(s) failed" >&2
  exit 1
fi

echo "awscli bundle tests OK"
