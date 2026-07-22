#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Single source of truth for Volume Plugin host tools in Ubuntu container images.
# Build/packaging injects this file into each image's Docker context as
# docker-install-volume-deps.sh (do not maintain per-Dockerfile copies):
#   - CubeMaster/docker/Dockerfile   — COPY deploy/scripts/... (repo-root context; CI + build-cube-images.sh)
#   - Cubelet/Dockerfile             — COPY deploy/scripts/... (repo-root context; CI + build-cube-images.sh)
#   - one-click CubeMaster/Dockerfile — build-release-bundle.sh → package CubeMaster/
#
# Installs:
#   cosfs  — Cubelet Attach/Detach (FUSE)
#   coscmd — CubeMaster Create/Destroy (binary plugin)
#   jq     — binary plugin JSON parsing
#
# Run as root during image build. Not a substitute for
# examples/volume/cos/install-deps.sh on bare-metal hosts.
#
# Docs: https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md

set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

COSFS_RELEASE="${COSFS_RELEASE:-v1.0.25}"
COSFS_BASE_URL="${COSFS_BASE_URL:-https://github.com/tencentyun/cosfs/releases/download/${COSFS_RELEASE}}"

log() { printf '[docker-volume-deps] %s\n' "$*"; }

detect_ubuntu_cosfs_tag() {
  local ver="22"
  if [[ -f /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    ver="${VERSION_ID%%.*}"
  fi
  case "$ver" in
    14) echo "ubuntu14.04" ;;
    16) echo "ubuntu16.04" ;;
    18) echo "ubuntu18.04" ;;
    20) echo "ubuntu20.04" ;;
    22) echo "ubuntu22.04" ;;
    24) echo "ubuntu24.04" ;;
    *)
      if [[ "${ver}" -ge 24 ]]; then
        echo "ubuntu24.04"
      else
        echo "ubuntu22.04"
      fi
      ;;
  esac
}

install_jq() {
  log "install jq"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends jq
  jq --version
}

install_cosfs() {
  local tag url tmp deb arch
  arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
  case "${arch}" in
    amd64|x86_64) ;;
    arm64|aarch64)
      # Official cosfs releases ship amd64/x86_64 packages only
      # (https://github.com/tencentyun/cosfs/releases). Skip on arm until an
      # arm64 package or source build is available; COS Attach needs cosfs.
      log "skip cosfs on ${arch}: no official arm64 .deb (temporary)"
      return 0
      ;;
    *)
      echo "ERROR: unsupported architecture for cosfs: ${arch}" >&2
      exit 1
      ;;
  esac
  tag="$(detect_ubuntu_cosfs_tag)"
  url="${COSFS_BASE_URL}/cosfs_1.0.25-${tag}_amd64.deb"
  log "install cosfs (${tag}) from ${url}"
  # cosfs Ubuntu debs link libcurl-gnutls / libxml2; the .deb Depends are thin,
  # so install runtime libs explicitly (libcurl3-gnutls → libcurl3t64-gnutls on 24.04).
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    fuse ca-certificates curl libcurl3-gnutls libxml2
  tmp="$(mktemp -d)"
  deb="${tmp}/cosfs.deb"
  curl -fsSL "$url" -o "$deb"
  dpkg -i "$deb" || apt-get install -y -f
  rm -rf "$tmp"
  command -v cosfs >/dev/null 2>&1 || {
    echo "ERROR: cosfs not on PATH after package install" >&2
    exit 1
  }
  cosfs --version | head -1
}

install_coscmd() {
  # Container-global: pip into the image Python (no venv).
  log "install coscmd system-wide via pip"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    python3 python3-pip
  # Ubuntu 23.04+/PEP 668 needs --break-system-packages for distro Python.
  if ! python3 -m pip install -q --upgrade --break-system-packages coscmd; then
    python3 -m pip install -q --upgrade coscmd
  fi
  command -v coscmd >/dev/null 2>&1 || {
    echo "ERROR: coscmd not on PATH after pip install" >&2
    exit 1
  }
  coscmd --version | head -1
}

main() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  install_jq
  install_cosfs
  install_coscmd
  apt-get clean
  rm -rf /var/lib/apt/lists/*
  local cosfs_path
  cosfs_path="$(command -v cosfs 2>/dev/null || echo '(skipped)')"
  log "installed: $(command -v jq) ${cosfs_path} $(command -v coscmd)"
}

main "$@"
