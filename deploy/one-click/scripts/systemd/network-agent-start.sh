#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

NETWORK_AGENT_BIN="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
NETWORK_AGENT_CFG="${TOOLBOX_ROOT}/network-agent/network-agent.yaml"
NETWORK_AGENT_STATE_DIR="/data/cubelet/network-agent/state"
CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"

ensure_executable "${NETWORK_AGENT_BIN}"
ensure_file "${NETWORK_AGENT_CFG}"
ensure_file "${CUBELET_CONFIG}"
mkdir -p /tmp/cube "${NETWORK_AGENT_STATE_DIR}"

exec "${NETWORK_AGENT_BIN}" --cubelet-config "${CUBELET_CONFIG}" --state-dir "${NETWORK_AGENT_STATE_DIR}"
