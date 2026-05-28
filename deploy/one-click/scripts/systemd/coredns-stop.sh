#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

COREDNS_CONTAINER="${CUBE_PROXY_COREDNS_CONTAINER:-cube-proxy-coredns}"
if container_exists "${COREDNS_CONTAINER}"; then
  # Graceful stop, then remove. Previously this only called `docker stop` and
  # left an Exited container behind; the next coredns-start.sh would clean it
  # up via docker_rm_if_exists, but in the meantime `docker ps -a` showed an
  # orphan and `systemctl stop ...control.target` looked dirty.
  docker stop -t 10 "${COREDNS_CONTAINER}" >/dev/null 2>&1 || true
  docker rm "${COREDNS_CONTAINER}" >/dev/null 2>&1 || true
fi
