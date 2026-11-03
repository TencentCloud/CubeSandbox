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

TPROXY_OK=false
for chain in TRANSPROXY CUBE_EGRESS; do
  if iptables -t mangle -n -L "${chain}" 2>/dev/null | grep -q "TPROXY"; then
    TPROXY_OK=true
    break
  fi
done
if [[ "${TPROXY_OK}" != true ]]; then
  echo "FAIL: no TPROXY rule found in mangle chains (checked TRANSPROXY and CUBE_EGRESS)" >&2
  exit 1
fi

echo "OK: ${INGRESS_IFACE} present, TPROXY rule live"
exit 0
