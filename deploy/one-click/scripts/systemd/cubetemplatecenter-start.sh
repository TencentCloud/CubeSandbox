#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

TC_BIN="${TOOLBOX_ROOT}/CubeTemplateCenter/bin/templatecenter"
TC_CFG="${TOOLBOX_ROOT}/CubeTemplateCenter/conf.yaml"

# TC's own settings are spelled CUBE_TEMPLATE_CENTER_*, matching the component
# name. The one exception is CubeMaster's address: that is CUBE_MASTER_ADDR, the
# same name every component uses, NOT a CUBE_TEMPLATE_CENTER_* name -- the
# address belongs to CubeMaster, not to TC. The retired spellings
# (CUBE_TEMPLATE_CENTER_MASTER_ENDPOINT, CUBE_MASTER_ENDPOINT, and the
# CUBE_TC_* / CUBEMASTER_* family) are still honoured by the binary as a
# fallback and log a deprecation notice, so an un-updated .one-click.env keeps
# working. See CubeTemplateCenter/pkg/tcconfig.

# Shared with cubemaster on purpose: TC writes the ext4, cubemaster serves the
# download (design 9.7). Reads the legacy CUBEMASTER_ name too, because
# .one-click.env may still define it for cubemaster's own local build mode, and
# the two MUST resolve to the same directory.
TC_ARTIFACT_STORE_DIR_DEFAULT="/data/CubeMaster/storage"
TC_ARTIFACT_STORE_DIR="${CUBE_TEMPLATE_CENTER_ARTIFACT_STORE_DIR:-${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR:-${TC_ARTIFACT_STORE_DIR_DEFAULT}}}"

# Where to report build results. Priority: env > yaml > default. CUBE_MASTER_ADDR
# is the canonical env name; the two retired TC-specific spellings are tried next
# so an old .one-click.env still works. If no env is set, conf.yaml's
# common.master_addr is used (set by the installer). Same host in the one-click
# layout, so the loopback default is correct; override in .one-click.env or
# conf.yaml for a split deployment.
TC_MASTER_ADDR="${CUBE_MASTER_ADDR:-${CUBE_TEMPLATE_CENTER_MASTER_ENDPOINT:-${CUBE_MASTER_ENDPOINT:-http://127.0.0.1:8089}}}"

# This node's routable address, used for the log identity and to narrow the HTTP
# listener when the address is actually assigned to this host. Defaults to the IP
# the installer already auto-detected for the rest of the stack, so a standard
# deployment needs no extra configuration.
TC_NODE_IP="${CUBE_TEMPLATE_CENTER_NODE_IP:-${CUBE_SANDBOX_NODE_IP:-}}"

# S3/MinIO artifact storage. TC reuses the SAME CUBE_S3_* variables that
# Cubelet / s3lvol / volume plugins already read, so a deployment configures S3
# once and every component picks it up. When CUBE_S3_ENDPOINT is absent or
# incomplete TC falls back to local disk storage.
#
# No extra export needed: the process reads CUBE_S3_* directly. This block is
# only a guard to warn when S3 is partially configured.
if [[ -n "${CUBE_S3_ENDPOINT:-}" && ( -z "${CUBE_S3_BUCKET:-}" || -z "${CUBE_S3_ACCESS_KEY:-}" || -z "${CUBE_S3_SECRET_KEY:-}" ) ]]; then
  log "warning: CUBE_S3_ENDPOINT is set but CUBE_S3_BUCKET / CUBE_S3_ACCESS_KEY / CUBE_S3_SECRET_KEY are incomplete; templatecenter will use local disk"
fi

ensure_executable "${TC_BIN}"
ensure_file "${TC_CFG}"

# Required to pull and unpack the source image, and to make the filesystem.
# Missing tools would otherwise surface much later as a failed build reported
# over the status callback, with a message that does not mention the real cause.
for tool in skopeo umoci mkfs.ext4; do
  command -v "${tool}" >/dev/null 2>&1 || die "templatecenter requires ${tool} in PATH"
done

export CUBE_TEMPLATE_CENTER_CONFIG_PATH="${TC_CFG}"
export CUBE_MASTER_ADDR="${TC_MASTER_ADDR}"
if [[ -n "${TC_NODE_IP}" ]]; then
  export CUBE_TEMPLATE_CENTER_NODE_IP="${TC_NODE_IP}"
fi

if mkdir -p "${TC_ARTIFACT_STORE_DIR}" >/dev/null 2>&1; then
  export CUBE_TEMPLATE_CENTER_ARTIFACT_STORE_DIR="${TC_ARTIFACT_STORE_DIR}"
else
  die "templatecenter artifact store ${TC_ARTIFACT_STORE_DIR} is not writable"
fi

exec "${TC_BIN}"
