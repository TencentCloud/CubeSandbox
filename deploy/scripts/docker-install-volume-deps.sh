#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Canonical installer for Volume Plugin host tools in Ubuntu container images.
# Keep copies in sync under:
#   deploy/one-click/CubeMaster/docker-install-volume-deps.sh
#   CubeMaster/docker/docker-install-volume-deps.sh
#   deploy/kubernetes/images/cubelet/docker-install-volume-deps.sh
#
# Installs:
#   cosfs  — Cubelet Attach/Detach (FUSE)
#   coscmd — CubeMaster Create/Destroy (binary plugin)
#   jq     — binary plugin JSON parsing
#
# Intended to be COPY'd into Docker build contexts and run as root during image build.
# Not a substitute for examples/volume/cos/install-deps.sh on bare-metal hosts.
#
# Docs: https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md

set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

COSFS_RELEASE="${COSFS_RELEASE:-v1.0.25}"
COSFS_BASE_URL="${COSFS_BASE_URL:-https://github.com/tencentyun/cosfs/releases/download/${COSFS_RELEASE}}"
COSCMD_VENV="${COSCMD_VENV:-/opt/coscmd-venv}"

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
  local tag url tmp deb
  tag="$(detect_ubuntu_cosfs_tag)"
  url="${COSFS_BASE_URL}/cosfs_1.0.25-${tag}_amd64.deb"
  log "install cosfs (${tag}) from ${url}"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    fuse ca-certificates curl
  tmp="$(mktemp -d)"
  deb="${tmp}/cosfs.deb"
  curl -fsSL "$url" -o "$deb"
  dpkg -i "$deb" || apt-get install -y -f
  rm -rf "$tmp"
  cosfs --version | head -1
}

install_coscmd() {
  log "install coscmd into ${COSCMD_VENV}"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    python3 python3-venv python3-pip
  python3 -m venv "${COSCMD_VENV}"
  "${COSCMD_VENV}/bin/pip" install -q --upgrade pip coscmd
  cat > /usr/local/bin/coscmd << EOF
#!/bin/bash
exec ${COSCMD_VENV}/bin/coscmd "\$@"
EOF
  chmod +x /usr/local/bin/coscmd
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
  log "installed: $(command -v jq) $(command -v cosfs) $(command -v coscmd)"
}

main "$@"
