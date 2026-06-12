#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Launch the cube-egress container in foreground (Type=simple unit).
#
# Network model is host (mandatory): nginx.conf binds explicitly to
# 192.168.0.1:8080/8443 with IP_TRANSPARENT, which only works in the
# host network namespace where the cube-dev iface lives.
#
# CA + audit dir come from cube-egress-prepare.sh (run as a oneshot
# ExecStartPre or independently by the operator).
# iptables/sysctl come from cube-sandbox-cube-egress-net.service.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

# Image resolution priority:
#   1. CUBE_SANDBOX_CUBE_EGRESS_IMAGE (explicit operator override)
#   2. MIRROR=cn  → cn.tencentcloudcr.com (China-region pull-through)
#   3. default    → int.tencentcloudcr.com (overseas/international)
#
# Production-pushed by CubeEgress/Makefile's `make push` to BOTH repos
# under the :v0.4.0 tag, so either default resolves to the same digest
# the operator most recently published.
CUBE_EGRESS_IMAGE_INT_DEFAULT="cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:v0.4.0"
CUBE_EGRESS_IMAGE_CN_DEFAULT="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-egress:v0.4.0"
if [[ -n "${CUBE_SANDBOX_CUBE_EGRESS_IMAGE:-}" ]]; then
  CUBE_EGRESS_IMAGE="${CUBE_SANDBOX_CUBE_EGRESS_IMAGE}"
elif [[ "${MIRROR:-}" == "cn" ]]; then
  CUBE_EGRESS_IMAGE="${CUBE_EGRESS_IMAGE_CN_DEFAULT}"
else
  CUBE_EGRESS_IMAGE="${CUBE_EGRESS_IMAGE_INT_DEFAULT}"
fi
CUBE_EGRESS_CONTAINER="${CUBE_SANDBOX_CUBE_EGRESS_CONTAINER:-cube-egress}"
CA_DIR="${CUBE_EGRESS_CA_DIR:-/etc/cube/ca}"
AUDIT_DIR="${CUBE_EGRESS_AUDIT_DIR:-/data/log/cube-egress}"

# Bootstrap URL: where lua/bootstrap.lua reaches for an initial policy
# dump on worker startup. network-agent listens on 127.0.0.1:19090 by
# default (NETWORK_AGENT_HEALTH_ADDR in up.sh); --network=host means
# this URL is reachable from inside the container without any port
# forwarding.
BOOTSTRAP_URL="${CUBE_EGRESS_BOOTSTRAP_URL:-http://127.0.0.1:19090/v1/policies/dump}"

ensure_dir "${CA_DIR}"
ensure_dir "${AUDIT_DIR}"
ensure_file "${CA_DIR}/cube-root-ca.crt"
ensure_file "${CA_DIR}/cube-root-ca.key"
ensure_file "${CA_DIR}/placeholder.crt"
ensure_file "${CA_DIR}/placeholder.key"

# Pull explicitly so the failure mode (registry unreachable / not
# logged in / image not pushed yet) surfaces here instead of midway
# through `docker create`. If the image already exists locally and
# the registry is unreachable, fall back to the cached copy.
log "pulling image ${CUBE_EGRESS_IMAGE}"
if ! docker pull "${CUBE_EGRESS_IMAGE}" >/dev/null; then
  if docker_image_exists "${CUBE_EGRESS_IMAGE}"; then
    log "WARN: pull failed but local image exists; using cached copy"
  else
    die "pull failed for ${CUBE_EGRESS_IMAGE} and no local copy is cached; check registry reachability and credentials (or set MIRROR=cn for the China-region registry)"
  fi
fi

docker_rm_if_exists "${CUBE_EGRESS_CONTAINER}"

# tmpfs for /var/run/openresty: nginx master mkdir's its temp dirs
# there at startup with CAP_CHOWN; image-time ownership is irrelevant.
# Capability set is a tight subset:
#   NET_ADMIN          IP_TRANSPARENT setsockopt
#   NET_RAW            raw sockets (TPROXY listener path)
#   NET_BIND_SERVICE   bind <1024 not actually needed but harmless;
#                      kept to match the operator-pasted command
#   CHOWN/SETUID/SETGID  master->worker uid 8049 transition
#   DAC_READ_SEARCH    read CA key under root:8049 mode 0640 from
#                      uid 0 (DAC_OVERRIDE would be simpler but
#                      DAC_READ_SEARCH is the principled minimum)
docker create \
  --name "${CUBE_EGRESS_CONTAINER}" \
  --network=host \
  --tmpfs /var/run/openresty:rw,nosuid,nodev,size=64m \
  -v "${CA_DIR}:/etc/cube/ca:ro" \
  -v "${AUDIT_DIR}:/data/log/cube-egress" \
  --cap-drop=ALL \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --cap-add=NET_BIND_SERVICE \
  --cap-add=CHOWN \
  --cap-add=SETUID \
  --cap-add=SETGID \
  --cap-add=DAC_READ_SEARCH \
  -e "CUBE_EGRESS_BOOTSTRAP_URL=${BOOTSTRAP_URL}" \
  "${CUBE_EGRESS_IMAGE}" >/dev/null

# `docker start -a` keeps the foreground attached to the container
# stdout/stderr; systemd captures it as the unit's journal.
exec docker start -a "${CUBE_EGRESS_CONTAINER}"
