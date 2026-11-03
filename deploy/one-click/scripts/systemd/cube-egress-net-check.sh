#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Readiness check for cube-egress host network rules. Exit 0 only when
# the TPROXY mangle chain is live. Use with:
#   systemctl show cube-sandbox-cube-egress-net -p StatusText
# or directly as a health-check command.

set -euo pipefail

INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"

if ! ip link show "${INGRESS_IFACE}" >/dev/null 2>&1; then
  echo "FAIL: ${INGRESS_IFACE} not present" >&2
  exit 1
fi

if ! iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q "TPROXY"; then
  echo "FAIL: CUBE_EGRESS mangle chain missing TPROXY rule" >&2
  exit 1
fi

echo "OK: ${INGRESS_IFACE} present, TPROXY rule live"
exit 0
