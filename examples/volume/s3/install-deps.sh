#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# install-deps.sh — install host tools for the S3 Volume Plugin.
#
#   --s3fs   FUSE mount driver          (Cubelet nodes; attach/detach)
#   --aws    AWS CLI v2                 (CubeMaster nodes; create/destroy)
#   --jq     JSON parsing               (both; binary plugin stdout)
#   --all    everything above           (single-host deployments)
#   --check-only   verify, install nothing
#   --print-aws-source  print zip source (bundle path or download URL) and exit
#
# All three tools ship for amd64 and arm64, so this script works unchanged on
# ARM64 Cube clusters.
#
# Usage:
#   sudo ./install-deps.sh --s3fs --jq        # Cubelet node
#   sudo ./install-deps.sh --aws --jq         # CubeMaster node
#   sudo ./install-deps.sh --all              # both roles on one host
#   ./install-deps.sh --all --check-only      # no root needed
#   AWSCLI_BUNDLE_ZIP=/path/to.zip ./install-deps.sh --print-aws-source

set -euo pipefail

WANT_S3FS=0
WANT_AWS=0
WANT_JQ=0
CHECK_ONLY=0
PRINT_AWS_SOURCE=0

# Keep in sync with deploy/one-click/assets/vendor/awscli/VERSION.
AWSCLI_VERSION="${AWSCLI_VERSION:-2.36.30}"

log()  { printf '[s3-deps] %s\n' "$*"; }
die()  { printf '[s3-deps] ERROR: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --s3fs)       WANT_S3FS=1; shift ;;
        --aws)        WANT_AWS=1;  shift ;;
        --jq)         WANT_JQ=1;   shift ;;
        --all)        WANT_S3FS=1; WANT_AWS=1; WANT_JQ=1; shift ;;
        --check-only) CHECK_ONLY=1; shift ;;
        --print-aws-source) PRINT_AWS_SOURCE=1; shift ;;
        -h|--help)    sed -n '4,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)            die "unknown argument: $1" ;;
    esac
done

if [[ "$WANT_S3FS$WANT_AWS$WANT_JQ" == "000" && "$PRINT_AWS_SOURCE" -eq 0 ]]; then
    die "nothing selected; pass --s3fs / --aws / --jq / --all (see --help)"
fi

if [[ "$CHECK_ONLY" -eq 0 && "$PRINT_AWS_SOURCE" -eq 0 && "$(id -u)" -ne 0 ]]; then
    die "must run as root to install (or pass --check-only)"
fi

# ---------------------------------------------------------------------------
# Package manager detection
# ---------------------------------------------------------------------------

PKG=""
if command -v apt-get >/dev/null 2>&1; then
    PKG="apt"
elif command -v dnf >/dev/null 2>&1; then
    PKG="dnf"
elif command -v yum >/dev/null 2>&1; then
    PKG="yum"
fi

ARCH="$(uname -m)"
if [[ "$PRINT_AWS_SOURCE" -eq 0 ]]; then
    log "host arch: ${ARCH}, package manager: ${PKG:-none}"
fi

aws_linux_arch() {
    case "$ARCH" in
        x86_64|amd64)  printf '%s\n' "x86_64" ;;
        aarch64|arm64) printf '%s\n' "aarch64" ;;
        *)             die "unsupported architecture for AWS CLI: ${ARCH}" ;;
    esac
}

# Prints "bundle<TAB>path" or "download<TAB>url". Prefers a packaged zip so
# one-click control nodes never hit awscli.amazonaws.com.
resolve_aws_zip() {
    local arch zip script_dir
    arch="$(aws_linux_arch)"
    if [[ -n "${AWSCLI_BUNDLE_ZIP:-}" ]]; then
        [[ -f "${AWSCLI_BUNDLE_ZIP}" ]] || die "AWSCLI_BUNDLE_ZIP not found: ${AWSCLI_BUNDLE_ZIP}"
        printf 'bundle\t%s\n' "${AWSCLI_BUNDLE_ZIP}"
        return 0
    fi
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    zip="${script_dir}/../../support/vendor/awscli/awscli-exe-linux-${arch}.zip"
    if [[ -f "${zip}" ]]; then
        printf 'bundle\t%s\n' "$(cd "$(dirname "${zip}")" && pwd)/$(basename "${zip}")"
        return 0
    fi
    printf 'download\thttps://awscli.amazonaws.com/awscli-exe-linux-%s-%s.zip\n' \
        "${arch}" "${AWSCLI_VERSION}"
}

if [[ "$PRINT_AWS_SOURCE" -eq 1 ]]; then
    resolve_aws_zip
    exit 0
fi

pkg_install() {
    case "$PKG" in
        apt) DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$@" ;;
        dnf) dnf install -y "$@" ;;
        yum) yum install -y "$@" ;;
        *)   die "unsupported package manager; install manually: $*" ;;
    esac
}

APT_UPDATED=0
pkg_refresh() {
    if [[ "$PKG" == "apt" && "$APT_UPDATED" -eq 0 ]]; then
        apt-get update -y
        APT_UPDATED=1
    fi
}

# ---------------------------------------------------------------------------
# Installers
# ---------------------------------------------------------------------------

install_jq() {
    log "install jq"
    pkg_refresh
    pkg_install jq
}

install_s3fs() {
    log "install s3fs"
    pkg_refresh
    case "$PKG" in
        # Debian/Ubuntu build the s3fs-fuse source package as binary "s3fs"
        # for both amd64 and arm64. s3fs's own deps pull the right fuse
        # package; installing "fuse" explicitly breaks on fuse3-only distros.
        apt)      pkg_install s3fs ;;
        # RHEL-family packages it as s3fs-fuse, from EPEL.
        dnf|yum)  pkg_install s3fs-fuse || {
                      log "s3fs-fuse not found; EPEL may be missing"
                      log "try: ${PKG} install -y epel-release && ${PKG} install -y s3fs-fuse"
                      return 1
                  } ;;
        *)        die "install s3fs manually: https://github.com/s3fs-fuse/s3fs-fuse" ;;
    esac
}

install_aws() {
    if command -v aws >/dev/null 2>&1 && aws --version >/dev/null 2>&1; then
        log "AWS CLI already installed: $(aws --version 2>&1)"
        return 0
    fi

    log "install AWS CLI v2"
    local kind src tmp
    IFS=$'\t' read -r kind src < <(resolve_aws_zip)

    command -v unzip >/dev/null 2>&1 || { pkg_refresh; pkg_install unzip; }

    tmp="$(mktemp -d)"
    if [[ "${kind}" == "bundle" ]]; then
        log "using bundled zip ${src}"
        unzip -q "${src}" -d "${tmp}"
    else
        command -v curl >/dev/null 2>&1 || { pkg_refresh; pkg_install curl; }
        log "downloading ${src}"
        curl -fsSL "${src}" -o "${tmp}/awscliv2.zip"
        unzip -q "${tmp}/awscliv2.zip" -d "${tmp}"
    fi
    "${tmp}/aws/install" --update
    rm -rf "${tmp}"
}

# ---------------------------------------------------------------------------
# Checks — run on the node that needs the tool
# ---------------------------------------------------------------------------

FAILED=0

check_jq() {
    if command -v jq >/dev/null 2>&1; then
        log "OK  jq        $(jq --version 2>&1)"
    else
        log "MISSING jq"; FAILED=1
    fi
}

check_s3fs() {
    if command -v s3fs >/dev/null 2>&1; then
        log "OK  s3fs      $(s3fs --version 2>&1 | head -1 || true)"
    else
        log "MISSING s3fs"; FAILED=1
    fi
    # Attach cannot mount without the FUSE device node.
    if [[ -e /dev/fuse ]]; then
        log "OK  /dev/fuse present"
    else
        log "MISSING /dev/fuse — attach will fail (load the fuse module)"; FAILED=1
    fi
}

check_aws() {
    if command -v aws >/dev/null 2>&1; then
        log "OK  aws       $(aws --version 2>&1)"
    else
        log "MISSING aws"; FAILED=1
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if [[ "$CHECK_ONLY" -eq 0 ]]; then
    if [[ "$WANT_JQ"   -eq 1 ]]; then install_jq;   fi
    if [[ "$WANT_S3FS" -eq 1 ]]; then install_s3fs; fi
    if [[ "$WANT_AWS"  -eq 1 ]]; then install_aws;  fi
fi

log "--- verification ---"
if [[ "$WANT_JQ"   -eq 1 ]]; then check_jq;   fi
if [[ "$WANT_S3FS" -eq 1 ]]; then check_s3fs; fi
if [[ "$WANT_AWS"  -eq 1 ]]; then check_aws;  fi

if [[ "$FAILED" -ne 0 ]]; then
    die "some dependencies are missing (see above)"
fi

log "all selected dependencies present"
