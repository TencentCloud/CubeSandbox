#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Sourced helpers for pinning, caching, and staging the official AWS CLI v2
# zip into the one-click package. Do not set shell options here.

if ! declare -F die >/dev/null 2>&1; then
  echo "awscli-bundle.sh must be sourced after lib/common.sh" >&2
  return 1 2>/dev/null || exit 1
fi

AWSCLI_BUNDLE_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AWSCLI_BUNDLE_ONE_CLICK_DIR="$(cd "${AWSCLI_BUNDLE_LIB_DIR}/.." && pwd)"

awscli_bundle_default_version() {
  local vf="${AWSCLI_BUNDLE_ONE_CLICK_DIR}/assets/vendor/awscli/VERSION"
  if [[ -f "${vf}" ]]; then
    tr -d '[:space:]' < "${vf}"
    return 0
  fi
  printf '%s\n' "2.36.30"
}

awscli_bundle_version() {
  printf '%s\n' "${ONE_CLICK_AWSCLI_VERSION:-$(awscli_bundle_default_version)}"
}

awscli_bundle_linux_arch() {
  local m="${ONE_CLICK_AWSCLI_ARCH:-$(uname -m)}"
  case "${m}" in
    x86_64|amd64) printf '%s\n' "x86_64" ;;
    aarch64|arm64) printf '%s\n' "aarch64" ;;
    *) die "unsupported architecture for AWS CLI: ${m}" ;;
  esac
}

awscli_bundle_cache_dir() {
  printf '%s\n' "${ONE_CLICK_AWSCLI_CACHE_DIR:-${AWSCLI_BUNDLE_ONE_CLICK_DIR}/assets/vendor/awscli}"
}

awscli_bundle_sha256sums() {
  printf '%s\n' "${ONE_CLICK_AWSCLI_SHA256SUMS:-$(awscli_bundle_cache_dir)/SHA256SUMS}"
}

awscli_bundle_filename() {
  local arch="${1:-$(awscli_bundle_linux_arch)}"
  printf 'awscli-exe-linux-%s-%s.zip\n' "${arch}" "$(awscli_bundle_version)"
}

awscli_bundle_url() {
  printf 'https://awscli.amazonaws.com/%s\n' "$(awscli_bundle_filename "${1:-}")"
}

awscli_bundle_expected_sha256() {
  local filename="$1"
  local sums line digest
  sums="$(awscli_bundle_sha256sums)"
  [[ -f "${sums}" ]] || die "AWS CLI SHA256SUMS not found: ${sums}"
  line="$(awk -v f="${filename}" '$2 == f { print; exit }' "${sums}")"
  [[ -n "${line}" ]] || die "no sha256 for ${filename} in ${sums}"
  digest="$(awk '{ print $1 }' <<<"${line}")"
  [[ -n "${digest}" ]] || die "empty sha256 for ${filename} in ${sums}"
  printf '%s\n' "${digest}"
}

awscli_bundle_verify_sha256() {
  local path="$1"
  local filename="$2"
  local expected actual
  expected="$(awscli_bundle_expected_sha256 "${filename}")"
  actual="$(file_sha256_hex "${path}")" || die "cannot hash ${path}"
  [[ "${actual}" == "${expected}" ]]
}

# Print the absolute zip path. Cache hit + matching sha256 does not touch the
# network. Never deletes deploy/one-click/assets/vendor/awscli/.
resolve_awscli_bundle_zip() {
  local arch filename cache_dir cache_zip url tmp curl_bin
  arch="$(awscli_bundle_linux_arch)"
  filename="$(awscli_bundle_filename "${arch}")"
  cache_dir="$(awscli_bundle_cache_dir)"
  mkdir -p "${cache_dir}"
  cache_zip="${cache_dir}/${filename}"

  if [[ -n "${ONE_CLICK_AWSCLI_ZIP:-}" ]]; then
    [[ -f "${ONE_CLICK_AWSCLI_ZIP}" ]] || die "ONE_CLICK_AWSCLI_ZIP not found: ${ONE_CLICK_AWSCLI_ZIP}"
    if ! awscli_bundle_verify_sha256 "${ONE_CLICK_AWSCLI_ZIP}" "${filename}"; then
      die "ONE_CLICK_AWSCLI_ZIP sha256 mismatch for ${filename}"
    fi
    log "using AWS CLI zip override ${ONE_CLICK_AWSCLI_ZIP}"
    printf '%s\n' "${ONE_CLICK_AWSCLI_ZIP}"
    return 0
  fi

  if [[ -f "${cache_zip}" ]] && awscli_bundle_verify_sha256 "${cache_zip}" "${filename}"; then
    log "using cached AWS CLI zip ${cache_zip}"
    printf '%s\n' "${cache_zip}"
    return 0
  fi

  if [[ -f "${cache_zip}" ]]; then
    log "cached AWS CLI zip sha256 mismatch; re-downloading ${filename}"
    rm -f "${cache_zip}"
  fi

  url="$(awscli_bundle_url "${arch}")"
  curl_bin="${AWSCLI_FETCH_CURL:-curl}"
  command -v "${curl_bin}" >/dev/null 2>&1 || die "required command not found: ${curl_bin}"
  tmp="$(mktemp "${cache_dir}/.${filename}.XXXXXX")"
  log "downloading AWS CLI ${url}"
  if ! "${curl_bin}" -fsSL --retry 3 -o "${tmp}" "${url}"; then
    rm -f "${tmp}"
    die "failed to download AWS CLI from ${url}"
  fi
  if ! awscli_bundle_verify_sha256 "${tmp}" "${filename}"; then
    rm -f "${tmp}"
    die "downloaded AWS CLI zip sha256 mismatch for ${filename}"
  fi
  mv -f "${tmp}" "${cache_zip}"
  printf '%s\n' "${cache_zip}"
}

# Copy the resolved zip into sandbox-package as a stable arch-named file.
stage_awscli_bundle_into_package() {
  local package_root="$1"
  local zip dest_dir dest arch
  [[ -n "${package_root}" ]] || die "stage_awscli_bundle_into_package: package root is empty"
  zip="$(resolve_awscli_bundle_zip)"
  arch="$(awscli_bundle_linux_arch)"
  dest_dir="${package_root}/support/vendor/awscli"
  dest="${dest_dir}/awscli-exe-linux-${arch}.zip"
  mkdir -p "${dest_dir}"
  copy_file "${zip}" "${dest}"
  printf '%s\n' "$(awscli_bundle_version)" > "${dest_dir}/VERSION"
  log "staged AWS CLI v$(awscli_bundle_version) at ${dest}"
}
