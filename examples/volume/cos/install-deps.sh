#!/usr/bin/env bash
# Install Tencent Cloud COS dependencies for CubeSandbox volume plugins.
#
# Packaged under CubeMaster/plugin/ and Cubelet/plugin/ in release bundles.
# Source: examples/volume/cos/install-deps.sh
#
# Where to run (multi-node):
#   cosfs  → Cubelet                attach/detach   binary + rpc
#   coscmd → CubeMaster             create/destroy  binary only
#   jq     → CubeMaster + Cubelet   binary JSON     binary only
#
# Usage:
#   sudo .../Cubelet/plugin/install-deps.sh --cosfs --jq
#   sudo .../CubeMaster/plugin/install-deps.sh --coscmd --jq
#   sudo .../Cubelet/plugin/install-deps.sh --all           # single-node
#   ./install-deps.sh --all --check-only
#
# ARM/aarch64 is not supported (--cosfs refuses); official cosfs is x86_64/amd64 only.
#
# Supported families (auto-detected via /etc/os-release):
#   RHEL/CentOS/TencentOS/Rocky/Alma 7/8/9  — cosfs RPM + yum/dnf
#   Ubuntu 14.04–24.04                      — cosfs .deb + apt
#   Debian                                  — cosfs .deb (ubuntu22.04 build) + apt
#
# EL OpenSSL note:
#   Official cosfs RPMs link older OpenSSL sonames. On EL8+/TencentOS 4 (OpenSSL 3)
#   install the matching compat package for the RPM we ship — do NOT blindly
#   install compat-openssl* on every distro (apt has no such packages).
#   centos7 RPM → libcrypto.so.10 → compat-openssl10
#   centos8 RPM → libcrypto.so.1.1 → compat-openssl11
#   Prefer PLATFORM_ID over VERSION_ID when present:
#     platform:elN  → treat as EL N (e.g. el9)
#     platform:tlN  → TencentOS Server N (e.g. tl4); treat as modern EL8+ cosfs
#
# Official docs:
#   cosfs:  https://cloud.tencent.com/document/product/436/6883
#   coscmd: https://cloud.tencent.com/document/product/436/10976

set -euo pipefail

COSFS_DOC="https://cloud.tencent.com/document/product/436/6883"
COSCMD_DOC="https://cloud.tencent.com/document/product/436/10976"
COSFS_RELEASE="v1.0.25"
COSFS_BASE_URL="https://github.com/tencentyun/cosfs/releases/download/${COSFS_RELEASE}"

INSTALL_COSFS=0
INSTALL_COSCMD=0
INSTALL_JQ=0
CHECK_ONLY=0
# el7 | el8 — which official cosfs RPM flavor we install (EL family only).
COSFS_EL_FLAVOR=""

usage() {
  sed -n '2,24p' "$0"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cosfs)      INSTALL_COSFS=1; shift ;;
    --coscmd)     INSTALL_COSCMD=1; shift ;;
    --jq)         INSTALL_JQ=1; shift ;;
    --all)        INSTALL_COSFS=1; INSTALL_COSCMD=1; INSTALL_JQ=1; shift ;;
    --check-only) CHECK_ONLY=1; shift ;;
    -h|--help)    usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 1 ;;
  esac
done

if [[ "$INSTALL_COSFS$INSTALL_COSCMD$INSTALL_JQ" == "000" ]]; then
  echo "ERROR: specify at least one of --cosfs, --coscmd, --jq, --all" >&2
  usage 1
fi

log()  { printf '[install-deps] %s\n' "$*"; }
warn() { printf '[install-deps] WARN: %s\n' "$*" >&2; }
fail() { printf '[install-deps] FAIL: %s\n' "$*" >&2; FAILED=1; }

FAILED=0
OS_ID="" OS_VERSION="" OS_FAMILY="" PKG_MGR=""

need_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "ERROR: root required (try: sudo $0 ...)" >&2
    exit 1
  fi
}

detect_os() {
  OS_ID="unknown"
  OS_VERSION=""
  local platform_id=""
  if [[ -f /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-unknown}"
    OS_VERSION="${VERSION_ID:-}"
    platform_id="${PLATFORM_ID:-}"
    case "${ID_LIKE:-}" in
      *rhel*|*fedora*) [[ "$OS_ID" == "unknown" ]] && OS_ID="rhel" ;;
      *debian*) [[ "$OS_ID" == "ubuntu" ]] || OS_ID="${ID:-debian}" ;;
    esac
  elif [[ -f /etc/redhat-release ]]; then
    OS_ID="rhel"
    if grep -qi 'release 7' /etc/redhat-release 2>/dev/null; then
      OS_VERSION="7"
    elif grep -qi 'release 8' /etc/redhat-release 2>/dev/null; then
      OS_VERSION="8"
    fi
  fi

  case "$OS_ID" in
    centos|rhel|rocky|almalinux|tencentos|tlinux|opencloudos|anolis)
      OS_FAMILY="el"
      OS_VERSION="${OS_VERSION%%.*}"
      # Prefer PLATFORM_ID over VERSION_ID:
      #   platform:el9 → 9; platform:tl4 (TencentOS Server 4) → modern → 8
      # Plain VERSION_ID=4 would incorrectly select the centos7 cosfs RPM.
      if [[ "$platform_id" =~ ^platform:el([0-9]+)$ ]]; then
        OS_VERSION="${BASH_REMATCH[1]}"
        log "using PLATFORM_ID=${platform_id} → el${OS_VERSION}"
      elif [[ "$platform_id" =~ ^platform:tl([0-9]+)$ ]]; then
        OS_VERSION="8"
        log "using PLATFORM_ID=${platform_id} → el8 cosfs (TencentOS Server)"
      fi
      [[ -z "$OS_VERSION" ]] && OS_VERSION="8"
      if command -v dnf >/dev/null 2>&1; then
        PKG_MGR="dnf"
      else
        PKG_MGR="yum"
      fi
      ;;
    ubuntu)
      OS_FAMILY="ubuntu"
      OS_VERSION="${OS_VERSION%%.*}"
      PKG_MGR="apt"
      ;;
    debian)
      OS_FAMILY="debian"
      OS_VERSION="${OS_VERSION%%.*}"
      PKG_MGR="apt"
      ;;
    *)
      if command -v apt-get >/dev/null 2>&1; then
        OS_FAMILY="debian"
        PKG_MGR="apt"
      elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
        OS_FAMILY="el"
        OS_VERSION="${OS_VERSION:-8}"
        if [[ "$platform_id" =~ ^platform:el([0-9]+)$ ]]; then
          OS_VERSION="${BASH_REMATCH[1]}"
        elif [[ "$platform_id" =~ ^platform:tl([0-9]+)$ ]]; then
          OS_VERSION="8"
        fi
        PKG_MGR="$(command -v dnf >/dev/null 2>&1 && echo dnf || echo yum)"
      else
        OS_FAMILY="unknown"
        PKG_MGR=""
      fi
      warn "unrecognized OS ID=${OS_ID:-?}; best-effort install as family=${OS_FAMILY}"
      ;;
  esac
  log "detected OS: id=${OS_ID} version=${OS_VERSION:-?} family=${OS_FAMILY} pkg=${PKG_MGR:-none}"
}

pkg_install() {
  need_root
  case "$PKG_MGR" in
    dnf)  dnf install -y "$@" ;;
    yum)  yum install -y "$@" ;;
    apt)
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
      ;;
    *)
      echo "ERROR: no supported package manager (yum/dnf/apt)" >&2
      return 1
      ;;
  esac
}

cosfs_el_flavor() {
  # Returns el7 or el8 — which upstream cosfs RPM to use.
  local major="${OS_VERSION:-8}"
  if [[ "$major" -le 7 ]]; then
    echo "el7"
  else
    echo "el8"
  fi
}

cosfs_rpm_url() {
  # Caller must set COSFS_EL_FLAVOR in the current shell first (not inside $()
  # subshell, or the assignment would be lost).
  if [[ -z "${COSFS_EL_FLAVOR}" ]]; then
    COSFS_EL_FLAVOR="$(cosfs_el_flavor)"
  fi
  if [[ "$COSFS_EL_FLAVOR" == "el7" ]]; then
    echo "${COSFS_BASE_URL}/cosfs-1.0.25-centos7.0.x86_64.rpm"
  else
    echo "${COSFS_BASE_URL}/cosfs-1.0.25-centos8.0.x86_64.rpm"
  fi
}

# Install OpenSSL compat libs required by the chosen cosfs RPM (EL / yum|dnf only).
# Best-effort: missing package is OK on images that already ship the soname.
install_cosfs_openssl_compat() {
  local flavor="${1:-${COSFS_EL_FLAVOR:-$(cosfs_el_flavor)}}"
  local pkg
  case "$flavor" in
    el7) pkg="compat-openssl10" ;; # libcrypto.so.10
    *)   pkg="compat-openssl11" ;; # libcrypto.so.1.1
  esac
  log "ensuring OpenSSL compat for cosfs (${flavor} → ${pkg})"
  if pkg_install "$pkg" 2>/dev/null; then
    log "installed ${pkg}"
  else
    warn "${pkg} not available via ${PKG_MGR} (may be ok if soname already present)"
  fi
}

# If cosfs fails to start due to a missing libcrypto soname, try the matching
# compat package once more (covers images where the first install was skipped).
repair_cosfs_openssl_compat() {
  local err="$1"
  local pkg=""
  if [[ "$err" == *libcrypto.so.10* ]]; then
    pkg="compat-openssl10"
    COSFS_EL_FLAVOR="el7"
  elif [[ "$err" == *libcrypto.so.1.1* ]] || [[ "$err" == *libssl.so.1.1* ]]; then
    pkg="compat-openssl11"
    COSFS_EL_FLAVOR="el8"
  else
    return 1
  fi
  [[ "$OS_FAMILY" == "el" ]] || return 1
  log "cosfs missing shared lib; trying ${pkg}"
  pkg_install "$pkg" 2>/dev/null || return 1
  return 0
}

cosfs_deb_url() {
  local ver="${OS_VERSION:-22}"
  local tag
  case "$ver" in
    14) tag="ubuntu14.04" ;;
    16) tag="ubuntu16.04" ;;
    18) tag="ubuntu18.04" ;;
    20) tag="ubuntu20.04" ;;
    22) tag="ubuntu22.04" ;;
    24) tag="ubuntu24.04" ;;
    *)
      if [[ "$ver" -gt 24 ]]; then
        tag="ubuntu24.04"
        warn "Ubuntu ${ver} has no dedicated cosfs deb; using ${tag} build"
      elif [[ "$ver" -lt 14 ]]; then
        tag="ubuntu14.04"
        warn "Ubuntu ${ver} is old; using ${tag} cosfs deb"
      else
        tag="ubuntu22.04"
        warn "no exact cosfs deb for Ubuntu ${ver}; using ${tag} build"
      fi
      ;;
  esac
  echo "${COSFS_BASE_URL}/cosfs_1.0.25-${tag}_amd64.deb"
}

install_cosfs_el() {
  local url tmp rpm
  COSFS_EL_FLAVOR="$(cosfs_el_flavor)"
  url="$(cosfs_rpm_url)"
  tmp="$(mktemp -d)"
  rpm="${tmp}/cosfs.rpm"
  log "downloading cosfs RPM (${COSFS_EL_FLAVOR}): ${url}"
  curl -fsSL "$url" -o "$rpm"
  pkg_install libxml2 fuse curl
  install_cosfs_openssl_compat "$COSFS_EL_FLAVOR"
  rpm -ivh --nodeps "$rpm" 2>/dev/null || rpm -Uvh --nodeps "$rpm"
  rm -rf "$tmp"
}

install_cosfs_deb() {
  local url tmp deb
  if [[ "$OS_FAMILY" == "debian" ]]; then
    warn "no Debian-specific cosfs package; using ubuntu22.04 build — see ${COSFS_DOC}"
  fi
  url="$(cosfs_deb_url)"
  tmp="$(mktemp -d)"
  deb="${tmp}/cosfs.deb"
  log "downloading cosfs deb: ${url}"
  curl -fsSL "$url" -o "$deb"
  pkg_install fuse curl ca-certificates
  # Install deps first, then the local deb.
  apt-get install -y -f || true
  dpkg -i "$deb" || apt-get install -y -f
  rm -rf "$tmp"
}

install_cosfs() {
  local arch
  log "install cosfs (doc: ${COSFS_DOC})"
  if [[ "$CHECK_ONLY" -eq 1 ]]; then
    return 0
  fi
  # Official cosfs packages are amd64/x86_64 only; skip on arm temporarily.
  arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
  case "${arch}" in
    arm64|aarch64)
      warn "skip cosfs on ${arch}: no official arm64 package (temporary); see ${COSFS_DOC}"
      return 0
      ;;
  esac
  need_root
  if command -v cosfs >/dev/null 2>&1; then
    log "cosfs already on PATH; skipping install"
    return 0
  fi
  case "$OS_FAMILY" in
    el) install_cosfs_el ;;
    ubuntu|debian) install_cosfs_deb ;;
    *)
      echo "ERROR: cannot auto-install cosfs on OS family=${OS_FAMILY}; see ${COSFS_DOC}" >&2
      return 1
      ;;
  esac
}

install_coscmd() {
  log "install coscmd via venv (doc: ${COSCMD_DOC})"
  if [[ "$CHECK_ONLY" -eq 1 ]]; then
    return 0
  fi
  need_root
  if command -v coscmd >/dev/null 2>&1; then
    log "coscmd already on PATH; skipping install"
    return 0
  fi
  command -v python3 >/dev/null 2>&1 || {
    pkg_install python3 python3-venv python3-pip 2>/dev/null || pkg_install python3
  }
  python3 -m venv /opt/coscmd-venv
  /opt/coscmd-venv/bin/pip install -q --upgrade pip coscmd
  cat > /usr/local/bin/coscmd << 'EOF'
#!/bin/bash
exec /opt/coscmd-venv/bin/coscmd "$@"
EOF
  chmod +x /usr/local/bin/coscmd
}

install_jq() {
  log "install jq (binary plugin JSON output)"
  if [[ "$CHECK_ONLY" -eq 1 ]]; then
    return 0
  fi
  need_root
  if command -v jq >/dev/null 2>&1; then
    log "jq already on PATH; skipping install"
    return 0
  fi
  pkg_install jq
}

check_cosfs() {
  log "check cosfs ..."
  if ! command -v cosfs >/dev/null 2>&1; then
    fail "cosfs not found in PATH"
    return
  fi
  local err ver
  err="$(cosfs --version 2>&1)" && ver="$(printf '%s\n' "$err" | head -1)" || {
    err="$(cosfs --version 2>&1 || true)"
    if [[ "$CHECK_ONLY" -eq 0 ]] && repair_cosfs_openssl_compat "$err"; then
      err="$(cosfs --version 2>&1)" && ver="$(printf '%s\n' "$err" | head -1)" || true
    fi
  }
  if ! cosfs --version >/dev/null 2>&1; then
    ver="$(printf '%s\n' "${err:-}" | head -1)"
    fail "cosfs --version failed: ${ver}"
    if [[ "$err" == *libcrypto.so.10* ]]; then
      fail "missing libcrypto.so.10 — on EL/TencentOS try: ${PKG_MGR:-yum} install -y compat-openssl10"
    elif [[ "$err" == *libcrypto.so.1.1* ]] || [[ "$err" == *libssl.so.1.1* ]]; then
      fail "missing libcrypto.so.1.1 — on EL/TencentOS try: ${PKG_MGR:-yum} install -y compat-openssl11"
    fi
    return
  fi
  ver="$(cosfs --version 2>&1 | head -1)"
  log "  cosfs: OK (${ver})"
  if [[ -e /dev/fuse ]]; then
    log "  /dev/fuse: OK"
  else
    fail "/dev/fuse missing — FUSE unavailable; cosfs attach will not work"
  fi
}

check_coscmd() {
  log "check coscmd ..."
  if ! command -v coscmd >/dev/null 2>&1; then
    fail "coscmd not found in PATH"
    return
  fi
  local ver
  if ! ver="$(coscmd --version 2>&1 | head -1)"; then
    fail "coscmd --version failed"
    return
  fi
  log "  coscmd: OK (${ver})"
}

check_jq() {
  log "check jq ..."
  if ! command -v jq >/dev/null 2>&1; then
    fail "jq not found in PATH"
    return
  fi
  local ver out
  ver="$(jq --version 2>&1)"
  if ! out="$(printf '%s' '{"ok":true}' | jq -r '.ok' 2>&1)"; then
    fail "jq smoke test failed: ${out}"
    return
  fi
  if [[ "$out" != "true" ]]; then
    fail "jq smoke test unexpected output: ${out}"
    return
  fi
  log "  jq: OK (${ver}, JSON parse ok)"
}

require_x86_64_for_cosfs() {
  # Official cosfs releases publish only x86_64/amd64 packages; Node attach
  # for the COS volume plugin depends on cosfs, so ARM hosts are unsupported.
  local arch
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) return 0 ;;
    *)
      echo "ERROR: cosfs (required for COS volume attach) does not support architecture ${arch}; official packages are x86_64/amd64 only. See ${COSFS_DOC}" >&2
      exit 1
      ;;
  esac
}

# ── main ────────────────────────────────────────────────────────────────────

detect_os

if [[ "$INSTALL_COSFS" -eq 1 ]]; then
  require_x86_64_for_cosfs
fi

if [[ "$CHECK_ONLY" -eq 0 ]]; then
  [[ "$INSTALL_COSFS"  -eq 1 ]] && install_cosfs
  [[ "$INSTALL_COSCMD" -eq 1 ]] && install_coscmd
  [[ "$INSTALL_JQ"     -eq 1 ]] && install_jq
else
  log "check-only mode (skip install)"
fi

log "running post-install checks ..."
[[ "$INSTALL_COSFS"  -eq 1 ]] && check_cosfs
[[ "$INSTALL_COSCMD" -eq 1 ]] && check_coscmd
[[ "$INSTALL_JQ"     -eq 1 ]] && check_jq

if [[ "$FAILED" -ne 0 ]]; then
  echo "ERROR: one or more checks failed; see ${COSFS_DOC} / ${COSCMD_DOC} for manual install" >&2
  exit 1
fi

log "all requested checks passed"
