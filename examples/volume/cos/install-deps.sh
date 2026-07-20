#!/usr/bin/env bash
# Install Tencent Cloud COS dependencies for CubeSandbox volume plugins.
#
# Where to run (multi-node clusters):
#   cosfs  → Cubelet node(s)     attach/detach   binary + rpc
#   coscmd → CubeMaster node      create/destroy  binary only
#   jq     → CubeMaster node      binary JSON     binary only
#
# Usage:
#   ./install-deps.sh --all              # cosfs + coscmd + jq (typical binary demo)
#   ./install-deps.sh --cosfs            # Cubelet / attach side only
#   ./install-deps.sh --coscmd           # CubeMaster / create side only (binary plugin)
#   ./install-deps.sh --jq               # required by binary shell plugin
#   ./install-deps.sh --all --check-only # verify already-installed tools only
#
# Supported families (auto-detected via /etc/os-release):
#   RHEL/CentOS/TencentOS/Rocky/Alma 7/8/9  — cosfs RPM + yum/dnf
#   Ubuntu 14.04–24.04                      — cosfs .deb + apt
#   Debian                                  — cosfs .deb (ubuntu22.04 build) + apt
#
# Official docs (latest packages / manual install):
#   cosfs:  https://cloud.tencent.com/document/product/436/10976
#   coscmd: https://cloud.tencent.com/document/product/436/6883
#
# RPC plugin Controller uses COS Go SDK (go mod); no coscmd — see cos/rpc/README.md

set -euo pipefail

COSFS_DOC="https://cloud.tencent.com/document/product/436/10976"
COSCMD_DOC="https://cloud.tencent.com/document/product/436/6883"
COSFS_RELEASE="v1.0.25"
COSFS_BASE_URL="https://github.com/tencentyun/cosfs/releases/download/${COSFS_RELEASE}"

INSTALL_COSFS=0
INSTALL_COSCMD=0
INSTALL_JQ=0
CHECK_ONLY=0

usage() {
  sed -n '2,20p' "$0"
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
  if [[ -f /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-unknown}"
    OS_VERSION="${VERSION_ID:-}"
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

cosfs_rpm_url() {
  local major="${OS_VERSION:-8}"
  if [[ "$major" -le 7 ]]; then
    echo "${COSFS_BASE_URL}/cosfs-1.0.25-centos7.0.x86_64.rpm"
  else
    echo "${COSFS_BASE_URL}/cosfs-1.0.25-centos8.0.x86_64.rpm"
  fi
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
  url="$(cosfs_rpm_url)"
  tmp="$(mktemp -d)"
  rpm="${tmp}/cosfs.rpm"
  log "downloading cosfs RPM: ${url}"
  curl -fsSL "$url" -o "$rpm"
  pkg_install libxml2 fuse curl
  if [[ "${OS_VERSION:-8}" -ge 8 ]]; then
    pkg_install compat-openssl11 2>/dev/null || warn "compat-openssl11 not available (may be ok on this EL image)"
  fi
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
  log "install cosfs (doc: ${COSFS_DOC})"
  if [[ "$CHECK_ONLY" -eq 1 ]]; then
    return 0
  fi
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
  local ver
  if ! ver="$(cosfs --version 2>&1 | head -1)"; then
    fail "cosfs --version failed"
    return
  fi
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

# ── main ────────────────────────────────────────────────────────────────────

detect_os

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
